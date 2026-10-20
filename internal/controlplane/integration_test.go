package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

var testActor = contract.Actor{Kind: "ci", Subject: "integration-ci"}

func testPool(t *testing.T, migrate bool) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("EREGION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run isolated PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	name := "eregion_test_" + strings.TrimPrefix(string(id), "op_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	pool, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
		if err != nil {
			t.Errorf("could not remove test database %s", name)
		}
	})
	if migrate {
		if _, err := Migrate(ctx, pool); err != nil {
			t.Fatalf("%v: %v", err, errors.Unwrap(err))
		}
	}
	return pool, u.String()
}

func testRuntime(t *testing.T, pool *pgxpool.Pool, workers int) *Runtime {
	t.Helper()
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Concurrency: workers, TestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func startWorkers(t *testing.T, run *Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := run.Jobs.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := run.Jobs.StopAndCancel(stopCtx); err != nil {
			t.Error(err)
		}
	})
}

func submission(key string, input string) operations.Submission {
	return operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: key, Input: json.RawMessage(input)}
}

func await(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for durable state")
}

func TestIntegrationMigrations(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := context.Background()
	plan, err := PlanMigrations(ctx, pool)
	if err != nil || plan.Current != 0 || plan.RiverCurrent != 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if Ready(ctx, pool) == nil {
		t.Fatal("unmigrated database was ready")
	}
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			if _, err := Migrate(ctx, pool); err != nil {
				t.Errorf("concurrent migration: %v", err)
			}
		})
	}
	wait.Wait()
	if err := Ready(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE eregion_schema_migrations SET checksum='edited'"); err != nil {
		t.Fatal(err)
	}
	if err := Ready(ctx, pool); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("did not detect edited migration: %v", err)
	}
}

func TestIntegrationAtomicSubmissionAndDeduplication(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	original := run.Service.enqueue
	run.Service.enqueue = func(ctx context.Context, tx pgx.Tx, id contract.OperationID) (int64, error) {
		if _, err := original(ctx, tx, id); err != nil {
			return 0, err
		}
		return 0, errors.New("injected failure after River insertion")
	}
	if _, err := run.Service.Submit(ctx, submission("atomic", `{}`)); err == nil {
		t.Fatal("injected failure succeeded")
	}
	for _, table := range []string{"eregion_operations", "eregion_operation_events", "river_job"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial commit in %s: %d %v", table, count, err)
		}
	}
	run.Service.enqueue = original
	// A deferred constraint fails only at COMMIT, after the accepted event was
	// appended. The domain row, River job, and event must still roll back together.
	if _, err := pool.Exec(ctx, `CREATE TABLE test_commit_failure (operation_id text REFERENCES eregion_operations(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	run.Service.enqueue = func(ctx context.Context, tx pgx.Tx, id contract.OperationID) (int64, error) {
		jobID, err := original(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO test_commit_failure(operation_id) VALUES ('missing-operation')`)
		return jobID, err
	}
	if _, err := run.Service.Submit(ctx, submission("commit-failure", `{}`)); err == nil {
		t.Fatal("deferred commit failure succeeded")
	}
	for _, table := range []string{"eregion_operations", "eregion_operation_events", "river_job"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial commit in %s: %d %v", table, count, err)
		}
	}
	run.Service.enqueue = original
	var wait sync.WaitGroup
	ids := make(chan contract.OperationID, 12)
	for range 12 {
		wait.Go(func() {
			op, err := run.Service.Submit(ctx, submission("same-key", `{}`))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- op.ID
		})
	}
	wait.Wait()
	close(ids)
	var id contract.OperationID
	for next := range ids {
		if id != "" && next != id {
			t.Fatal("duplicate domain operations")
		}
		id = next
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate River jobs: %d %v", count, err)
	}
	op, err := run.Service.Submit(ctx, submission("same-key", `{"steps":3,"step_delay_ms":100,"fail_until_attempt":0}`))
	if err != nil || op.ID != id {
		t.Fatalf("equivalent normalized input did not deduplicate: %v", err)
	}
	if _, err := run.Service.Submit(ctx, submission("same-key", `{"steps":4}`)); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed input reused key: %v", err)
	}
	if _, err := run.Service.Get(ctx, contract.Actor{Kind: "ci", Subject: "other-ci"}, id); contract.Classify(err).Code != contract.NotFound {
		t.Fatal("cross-actor read exposed operation")
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: id}, 100)
	if err != nil || len(page.Events) != 1 || page.Events[0].State != contract.Queued {
		t.Fatalf("missing atomic accepted event: %+v %v", page, err)
	}
}

func TestIntegrationAPIWorkersAndReplay(t *testing.T) {
	pool, _ := testPool(t, true)
	apiRun := testRuntime(t, pool, 0)
	token := strings.Repeat("d", 64)
	hash := sha256.Sum256([]byte(token))
	auth, err := api.NewAuth([]api.Principal{{Actor: testActor, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewHandler(auth, apiRun.Service, func(ctx context.Context) error { return Ready(ctx, pool) }))
	t.Cleanup(server.Close)
	client, err := api.NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw, err := client.Test(ctx, operations.TestInput{Steps: 6, StepDelayMS: 25, FailUntilAttempt: 1}, "api-replay", false)
	if err != nil {
		t.Fatal(err)
	}
	var submitted contract.Operation
	if err := json.Unmarshal(raw, &submitted); err != nil || submitted.State != contract.Queued {
		t.Fatalf("not accepted: %s %v", raw, err)
	}
	// The submitting connection/client can disappear; a new client finds the same op.
	client, _ = api.NewClient(server.URL, token)
	startWorkers(t, testRuntime(t, pool, 2))
	startWorkers(t, testRuntime(t, pool, 2))
	await(t, 15*time.Second, func() bool {
		op, err := apiRun.Service.Get(ctx, testActor, submitted.ID)
		return err == nil && op.State == contract.Succeeded
	})
	op, _ := apiRun.Service.Get(ctx, testActor, submitted.ID)
	if op.Attempt != 2 {
		t.Fatalf("retry attempt not durable: %+v", op)
	}
	cursor := contract.Cursor{OperationID: submitted.ID}
	progress := 0
	for cursor.After < op.LastSequence {
		raw, err := client.Events(ctx, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		var page operations.EventPage
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) == 0 {
			t.Fatal("replay stalled")
		}
		for _, event := range page.Events {
			if event.Sequence != cursor.After+1 {
				t.Fatal("noncontiguous replay")
			}
			cursor.After = event.Sequence
			if event.Kind == "progress" {
				progress++
			}
		}
		if page.Cursor != cursor {
			t.Fatal("incorrect resume cursor")
		}
	}
	if progress != 6 {
		t.Fatalf("duplicate/lost diagnostic steps: %d", progress)
	}
	if _, err := client.Events(ctx, contract.Cursor{OperationID: op.ID, After: op.LastSequence + 1}, 2); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatal("future cursor accepted")
	}
}

func TestIntegrationFailureCancellationAndReconciliation(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 2)
	ctx := context.Background()
	cancelled, err := run.Service.Submit(ctx, submission("cancel", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.Service.RequestCancel(ctx, testActor, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := run.Service.Submit(ctx, submission("fail", `{"fail_until_attempt":3}`))
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	await(t, 15*time.Second, func() bool {
		a, _ := run.Service.Get(ctx, testActor, cancelled.ID)
		b, _ := run.Service.Get(ctx, testActor, failed.ID)
		return a.State == contract.Canceled && b.State == contract.Failed
	})
	op, _ := run.Service.Get(ctx, testActor, failed.ID)
	if op.Attempt != 3 {
		t.Fatal("retry bound not enforced")
	}
	stopCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := run.Jobs.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	// Simulate River's exhausted-crash discard, which has no Work error callback.
	insertOnly := testRuntime(t, pool, 0)
	lost, err := insertOnly.Service.Submit(ctx, submission("lost-final-attempt", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state='discarded',finalized_at=clock_timestamp() WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1) AND state='available'`, lost.ID); err != nil {
		t.Fatal(err)
	}
	if err := insertOnly.Service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	op, _ = insertOnly.Service.Get(ctx, testActor, lost.ID)
	if op.State != contract.Failed {
		t.Fatalf("discard was invisible in domain history: %+v", op)
	}
}

func TestIntegrationCrashWorkerHelper(t *testing.T) {
	if os.Getenv("EREGION_CRASH_WORKER_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	pool, err := Open(context.Background(), os.Getenv("EREGION_CRASH_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	run := testRuntime(t, pool, 1)
	if err := run.Jobs.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestIntegrationWorkerProcessCrash(t *testing.T) {
	pool, dsn := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("crash-recovery", `{"steps":8,"step_delay_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestIntegrationCrashWorkerHelper$")
	child.Env = append(os.Environ(), "EREGION_CRASH_WORKER_HELPER=1", "EREGION_CRASH_DATABASE_URL="+dsn)
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	await(t, 10*time.Second, func() bool {
		var completed int
		err := pool.QueryRow(ctx, "SELECT completed_steps FROM eregion_operations WHERE id=$1", op.ID).Scan(&completed)
		return err == nil && completed > 0
	})
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	killed = true
	// Advance only the test database's stale timestamps instead of waiting for the
	// production two-minute rescue threshold and expired leader lease.
	if _, err := pool.Exec(ctx, `UPDATE river_job SET attempted_at=clock_timestamp()-interval '5 minutes' WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_leader SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	startWorkers(t, testRuntime(t, pool, 2))
	await(t, 45*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Succeeded
	})
	current, _ := run.Service.Get(ctx, testActor, op.ID)
	if current.Attempt < 2 {
		t.Fatal("crashed attempt was not rescued")
	}
	page, err := run.Service.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, event := range page.Events {
		if event.Kind == "progress" {
			count++
		}
	}
	if count != 8 {
		t.Fatalf("crash duplicated/lost committed steps: %d", count)
	}
}

func TestIntegrationExecutableLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("control-plane processes require Linux")
	}
	pool, dsn := testPool(t, true)
	binary := filepath.Join(t.TempDir(), "eregion")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/eregion").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	credential := strings.Repeat("e", 64)
	hash := sha256.Sum256([]byte(credential))
	authFile := filepath.Join(t.TempDir(), "auth.json")
	raw, _ := json.Marshal([]api.Principal{{Actor: testActor, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}}})
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	endpoint := "http://" + address
	environment := append(os.Environ(), "EREGION_DATABASE_URL="+dsn, "EREGION_AUTH_FILE="+authFile, "EREGION_API_URL="+endpoint, "EREGION_API_TOKEN="+credential)
	start := func(args ...string) (*exec.Cmd, <-chan error) {
		command := exec.Command(binary, args...)
		command.Env = environment
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		t.Cleanup(func() { _ = command.Process.Kill() })
		return command, done
	}
	server, serverDone := start("serve", "--workers=false", "--listen", address, "--json")
	await(t, 10*time.Second, func() bool {
		request, _ := http.NewRequest(http.MethodGet, endpoint+"/readyz", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		client := &http.Client{Timeout: time.Second}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == 200
	})
	command := exec.Command(binary, "operations", "test", "--steps", "5", "--step-delay-ms", "25", "--idempotency-key", "cli:durable-1", "--json")
	command.Env = environment
	raw, err = command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		OK   bool               `json:"ok"`
		Data contract.Operation `json:"data"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil || !accepted.OK || accepted.Data.State != contract.Queued {
		t.Fatalf("CLI did not durably queue: %s %v", raw, err)
	}
	worker, workerDone := start("worker", "--concurrency", "2", "--json")
	service := testRuntime(t, pool, 0).Service
	await(t, 10*time.Second, func() bool {
		op, err := service.Get(context.Background(), testActor, accepted.Data.ID)
		return err == nil && op.State == contract.Succeeded
	})
	command = exec.Command(binary, "operations", "events", string(accepted.Data.ID), "--limit", "2", "--json")
	command.Env = environment
	raw, err = command.Output()
	if err != nil || !json.Valid(raw) {
		t.Fatalf("CLI could not reconnect to events: %s %v", raw, err)
	}
	for _, process := range []struct {
		cmd  *exec.Cmd
		done <-chan error
	}{{worker, workerDone}, {server, serverDone}} {
		if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-process.done:
			if err != nil {
				t.Fatalf("unclean service shutdown: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("service ignored graceful shutdown")
		}
	}
}
