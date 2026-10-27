package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/sshfixture"
	"github.com/mbvlabs/eregion/internal/transport"
)

// startWireGuardFixture starts the disposable SSH node behind kernel
// WireGuard. The test skips when Docker, NET_ADMIN, or the wireguard module is
// not available.
func startWireGuardFixture(t *testing.T) *sshfixture.Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{WireGuard: true})
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the WireGuard fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })
	return fixture
}

// wireGuardDialerFor builds the dialer to the fixture node with short
// timings, so a lost tunnel is noticed within a couple of seconds.
func wireGuardDialerFor(keyFile, peerText, endpoint string) (*transport.WireGuardDialer, error) {
	key, err := transport.ReadWireGuardKey(keyFile)
	if err != nil {
		return nil, err
	}
	peer, err := transport.ParseWireGuardPublicKey(peerText)
	if err != nil {
		return nil, err
	}
	return transport.NewWireGuardDialer(transport.WireGuardConfig{
		PrivateKey:       key,
		Address:          netip.MustParseAddr("10.77.0.1"),
		NodeAddress:      netip.MustParseAddr("10.77.0.2"),
		PeerPublicKey:    peer,
		Endpoint:         endpoint,
		AllowedIPs:       []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")},
		Keepalive:        time.Second,
		HandshakeTimeout: 3 * time.Second,
		ProbeInterval:    200 * time.Millisecond,
		LossAfter:        1500 * time.Millisecond,
	})
}

// wireGuardTransport builds the pinned SSH transport inside the fixture's
// tunnel. endpoint is the node's WireGuard port or a relay in front of it.
func wireGuardTransport(t *testing.T, fixture *sshfixture.Fixture, endpoint string) (node.Transport, *transport.WireGuardDialer) {
	t.Helper()
	dialer, err := wireGuardDialerFor(fixture.WireGuard.PrivateKeyFile, fixture.WireGuard.PublicKey, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close() })
	keys, err := transport.LoadHostKeys(fixture.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	client, err := transport.NewSSHTransport(transport.SSHConfig{
		Address:      fixture.Address,
		User:         "eregion",
		IdentityFile: fixture.IdentityFile,
		HostKeys:     keys,
		ExpectedNode: "fixture-node",
		Timeout:      30 * time.Second,
		PollInterval: 100 * time.Millisecond,
		Dialer:       dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, dialer
}

// lockedBuffer collects a worker's log lines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wireGuardRuntime is a runtime whose worker log is captured, so a test can
// prove that the WireGuard key never reaches it.
func wireGuardRuntime(t *testing.T, pool *pgxpool.Pool, workers int, log io.Writer) *Runtime {
	t.Helper()
	run, err := New(pool, slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})), WorkerConfig{Concurrency: workers, TestOnly: true, NodeStateDir: t.TempDir(), LocalNodeName: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// assertKeyNotPersisted proves that the control plane's WireGuard private key
// is in no operation row, step row, event, River job, or worker log.
func assertKeyNotPersisted(t *testing.T, pool *pgxpool.Pool, fixture *sshfixture.Fixture, logs ...string) {
	t.Helper()
	raw, err := os.ReadFile(fixture.WireGuard.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(raw))
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	forms := []string{encoded, hex.EncodeToString(decoded)}
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND (table_name LIKE 'eregion\_%' OR table_name LIKE 'river\_%')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("no table was scanned")
	}
	for _, table := range tables {
		var dump string
		if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(t::text, E'\n'), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		for _, form := range forms {
			if strings.Contains(dump, form) {
				t.Fatalf("the WireGuard key is persisted in %s", table)
			}
		}
	}
	for _, log := range logs {
		for _, form := range forms {
			if strings.Contains(log, form) {
				t.Fatal("the WireGuard key reached the worker log")
			}
		}
	}
}

// TestIntegrationWireGuardRunsTheDiagnosticOperation proves that the control
// plane dispatches node work over SSH inside a userspace WireGuard tunnel and
// that the effect lands on the node exactly once per step.
func TestIntegrationWireGuardRunsTheDiagnosticOperation(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startWireGuardFixture(t)
	log := &lockedBuffer{}
	run := wireGuardRuntime(t, pool, 2, log)
	run.Service.transport, _ = wireGuardTransport(t, fixture, fixture.WireGuard.Endpoint)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("wg-diagnostic", `{"steps":2,"step_delay_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	await(t, 60*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Succeeded
	})
	count, err := fixture.EffectCount(ctx, string(op.ID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("the remote node wrote %d effects, want 2", count)
	}
	if got := completedStepRows(t, pool, op.ID); got != 2 {
		t.Fatalf("completed step rows: %d", got)
	}
	if got := progressEventCount(t, run.Service, op.ID); got != 2 {
		t.Fatalf("progress events: %d", got)
	}
	assertKeyNotPersisted(t, pool, fixture, log.String())
}

// TestIntegrationWireGuardRecoversFromALostTunnel proves that a tunnel lost
// after dispatch is an explicit unknown outcome: the node task keeps running,
// and the retry reconnects and recovers through status without applying the
// effect twice.
func TestIntegrationWireGuardRecoversFromALostTunnel(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startWireGuardFixture(t)
	relay, err := sshfixture.NewUDPRelay(fixture.WireGuard.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	log := &lockedBuffer{}
	run := wireGuardRuntime(t, pool, 2, log)
	var dialer *transport.WireGuardDialer
	run.Service.transport, dialer = wireGuardTransport(t, fixture, relay.Address())
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("wg-lost-tunnel", `{"steps":1,"step_delay_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	// The node task is dispatched and holds the effect: now the tunnel dies,
	// and comes back once the control plane noticed the loss.
	await(t, 30*time.Second, func() bool {
		pid, err := fixture.TaskOwnerPID(ctx, string(op.ID))
		return err == nil && pid != 0
	})
	relay.SetBlackhole(true)
	await(t, 20*time.Second, func() bool { return !dialer.Up() })
	relay.SetBlackhole(false)
	await(t, 90*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Succeeded
	})
	if relay.Dropped() == 0 {
		t.Fatal("the tunnel was never lost")
	}
	count, err := fixture.EffectCount(ctx, string(op.ID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the remote node wrote %d effects, want 1", count)
	}
	if got := completedStepRows(t, pool, op.ID); got != 1 {
		t.Fatalf("completed step rows: %d", got)
	}
	if got := progressEventCount(t, run.Service, op.ID); got != 1 {
		t.Fatalf("progress events: %d", got)
	}
	current, _ := run.Service.Get(ctx, testActor, op.ID)
	if current.Attempt < 2 {
		t.Fatalf("the lost tunnel did not cause a retry: attempt %d", current.Attempt)
	}
	assertKeyNotPersisted(t, pool, fixture, log.String())
}

// TestIntegrationWireGuardDuplicateSubmissionsShareOneEffect proves that
// duplicate submissions over the tunnel share one operation and one effect
// per step.
func TestIntegrationWireGuardDuplicateSubmissionsShareOneEffect(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startWireGuardFixture(t)
	log := &lockedBuffer{}
	run := wireGuardRuntime(t, pool, 2, log)
	run.Service.transport, _ = wireGuardTransport(t, fixture, fixture.WireGuard.Endpoint)
	ctx := context.Background()
	var wait sync.WaitGroup
	ids := make(chan contract.OperationID, 6)
	for range 6 {
		wait.Go(func() {
			op, err := run.Service.Submit(ctx, submission("wg-duplicate", `{"steps":2,"step_delay_ms":50}`))
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
			t.Fatalf("duplicate submissions created different operations: %s %s", id, next)
		}
		id = next
	}
	startWorkers(t, run)
	await(t, 60*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, id)
		return err == nil && current.State == contract.Succeeded
	})
	count, err := fixture.EffectCount(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("the remote node wrote %d effects, want 2", count)
	}
	if got := completedStepRows(t, pool, id); got != 2 {
		t.Fatalf("completed step rows: %d", got)
	}
	if got := progressEventCount(t, run.Service, id); got != 2 {
		t.Fatalf("progress events: %d", got)
	}
	assertKeyNotPersisted(t, pool, fixture, log.String())
}

// TestIntegrationWireGuardCancellationHonoursStepBoundary proves that a
// cancellation over the tunnel stops at the step boundary: the completed
// effect is acknowledged and no later effect is applied.
func TestIntegrationWireGuardCancellationHonoursStepBoundary(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startWireGuardFixture(t)
	log := &lockedBuffer{}
	run := wireGuardRuntime(t, pool, 2, log)
	run.Service.transport, _ = wireGuardTransport(t, fixture, fixture.WireGuard.Endpoint)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("wg-cancel", `{"steps":3,"step_delay_ms":300}`))
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	await(t, 30*time.Second, func() bool {
		var completed int
		err := pool.QueryRow(ctx, "SELECT completed_steps FROM eregion_operations WHERE id=$1", op.ID).Scan(&completed)
		return err == nil && completed > 0
	})
	if _, err := run.Service.RequestCancel(ctx, testActor, op.ID); err != nil {
		t.Fatal(err)
	}
	await(t, 60*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && (current.State == contract.Canceled || current.State == contract.Succeeded)
	})
	current, _ := run.Service.Get(ctx, testActor, op.ID)
	if current.State != contract.Canceled {
		t.Fatalf("cancellation did not stop the operation: %+v", current)
	}
	count, err := fixture.EffectCount(ctx, string(op.ID))
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || count > 2 {
		t.Fatalf("cancellation applied %d effects, want the completed steps only", count)
	}
	assertKeyNotPersisted(t, pool, fixture, log.String())
}

// TestIntegrationWireGuardWorkerProcessCrashRecoversExactlyOnce proves that a
// killed worker does not lose or duplicate an effect it dispatched through
// the tunnel: the node task outlives its caller, and a new worker with a new
// tunnel reconciles it.
func TestIntegrationWireGuardWorkerProcessCrashRecoversExactlyOnce(t *testing.T) {
	pool, dsn := testPool(t, true)
	fixture := startWireGuardFixture(t)
	nodeDir := t.TempDir()
	run := testRuntime(t, pool, 0, nodeDir)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("wg-crash", `{"steps":4,"step_delay_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestIntegrationCrashWorkerHelper$")
	var childLog lockedBuffer
	child.Env = append(os.Environ(),
		"EREGION_CRASH_WORKER_HELPER=1",
		"EREGION_CRASH_DATABASE_URL="+dsn,
		"EREGION_NODE_STATE_DIR="+nodeDir,
		"EREGION_CRASH_SSH_ADDRESS="+fixture.Address,
		"EREGION_CRASH_SSH_IDENTITY="+fixture.IdentityFile,
		"EREGION_CRASH_SSH_KNOWN_HOSTS="+fixture.KnownHostsFile,
		"EREGION_CRASH_WG_KEY_FILE="+fixture.WireGuard.PrivateKeyFile,
		"EREGION_CRASH_WG_PEER="+fixture.WireGuard.PublicKey,
		"EREGION_CRASH_WG_ENDPOINT="+fixture.WireGuard.Endpoint,
	)
	child.Stdout, child.Stderr = &childLog, &childLog
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
	await(t, 30*time.Second, func() bool {
		var completed int
		err := pool.QueryRow(ctx, "SELECT completed_steps FROM eregion_operations WHERE id=$1", op.ID).Scan(&completed)
		return err == nil && completed > 0
	})
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	killed = true
	if _, err := pool.Exec(ctx, `UPDATE river_job SET attempted_at=clock_timestamp()-interval '5 minutes' WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_leader SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	rescued := wireGuardRuntime(t, pool, 2, log)
	rescued.Service.transport, _ = wireGuardTransport(t, fixture, fixture.WireGuard.Endpoint)
	startWorkers(t, rescued)
	await(t, 90*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Succeeded
	})
	count, err := fixture.EffectCount(ctx, string(op.ID))
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("the remote node wrote %d effects, want 4", count)
	}
	if got := completedStepRows(t, pool, op.ID); got != 4 {
		t.Fatalf("completed step rows: %d", got)
	}
	if got := progressEventCount(t, run.Service, op.ID); got != 4 {
		t.Fatalf("progress events: %d", got)
	}
	assertKeyNotPersisted(t, pool, fixture, log.String(), childLog.String())
}
