package controlplane

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/sshfixture"
	"github.com/mbvlabs/eregion/internal/transport"
)

// startSSHFixture starts the disposable SSH node. The test skips when Docker or
// the fixture image is not available.
func startSSHFixture(t *testing.T) *sshfixture.Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixture, err := sshfixture.Start(ctx, sshfixture.Options{})
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })
	return fixture
}

// sshTransport builds the pinned SSH transport for the fixture node.
func sshTransport(t *testing.T, fixture *sshfixture.Fixture, address string) node.Transport {
	t.Helper()
	keys, err := transport.LoadHostKeys(fixture.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	// The address may be a proxy in front of the node. It presents the node's
	// host key, so the pin follows the address the client dials.
	keys.Pin(address, fixture.HostKey)
	client, err := transport.NewSSHTransport(transport.SSHConfig{
		Address:      address,
		User:         "eregion",
		IdentityFile: fixture.IdentityFile,
		HostKeys:     keys,
		ExpectedNode: "fixture-node",
		Timeout:      30 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// sshTransportFromEnvironment builds the SSH transport for a helper process.
func sshTransportFromEnvironment() (node.Transport, error) {
	keys, err := transport.LoadHostKeys(os.Getenv("EREGION_CRASH_SSH_KNOWN_HOSTS"))
	if err != nil {
		return nil, err
	}
	return transport.NewSSHTransport(transport.SSHConfig{
		Address:      os.Getenv("EREGION_CRASH_SSH_ADDRESS"),
		User:         "eregion",
		IdentityFile: os.Getenv("EREGION_CRASH_SSH_IDENTITY"),
		HostKeys:     keys,
		ExpectedNode: "fixture-node",
		Timeout:      30 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
}

// TestIntegrationSSHDuplicateSubmissionsShareOneEffect proves that duplicate
// submissions over a remote node share one operation and one effect per step.
func TestIntegrationSSHDuplicateSubmissionsShareOneEffect(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startSSHFixture(t)
	run := testRuntime(t, pool, 2)
	run.Service.transport = sshTransport(t, fixture, fixture.Address)
	ctx := context.Background()
	var wait sync.WaitGroup
	ids := make(chan contract.OperationID, 6)
	for range 6 {
		wait.Go(func() {
			op, err := run.Service.Submit(ctx, submission("ssh-duplicate", `{"steps":2,"step_delay_ms":50}`))
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
}

// TestIntegrationSSHCancellationHonoursStepBoundary proves that a cancellation
// over a remote node stops at the step boundary: the completed effect is
// acknowledged and no later effect is applied.
func TestIntegrationSSHCancellationHonoursStepBoundary(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startSSHFixture(t)
	run := testRuntime(t, pool, 2)
	run.Service.transport = sshTransport(t, fixture, fixture.Address)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("ssh-cancel", `{"steps":3,"step_delay_ms":300}`))
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
}

// TestIntegrationSSHWorkerProcessCrashRecoversExactlyOnce proves that a
// killed worker does not lose or duplicate a remote effect: the node task
// outlives its caller and the next attempt reconciles it.
func TestIntegrationSSHWorkerProcessCrashRecoversExactlyOnce(t *testing.T) {
	pool, dsn := testPool(t, true)
	fixture := startSSHFixture(t)
	nodeDir := t.TempDir()
	run := testRuntime(t, pool, 0, nodeDir)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("ssh-crash", `{"steps":4,"step_delay_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestIntegrationCrashWorkerHelper$")
	child.Env = append(os.Environ(),
		"EREGION_CRASH_WORKER_HELPER=1",
		"EREGION_CRASH_DATABASE_URL="+dsn,
		"EREGION_NODE_STATE_DIR="+nodeDir,
		"EREGION_CRASH_SSH_ADDRESS="+fixture.Address,
		"EREGION_CRASH_SSH_IDENTITY="+fixture.IdentityFile,
		"EREGION_CRASH_SSH_KNOWN_HOSTS="+fixture.KnownHostsFile,
	)
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
	rescued := testRuntime(t, pool, 2, nodeDir)
	rescued.Service.transport = sshTransport(t, fixture, fixture.Address)
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
}

// TestIntegrationSSHTransportRunsTheDiagnosticOperation proves that the control
// plane dispatches node work over pinned SSH and that the effect lands on the
// remote node exactly once per step.
func TestIntegrationSSHTransportRunsTheDiagnosticOperation(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startSSHFixture(t)
	run := testRuntime(t, pool, 2)
	run.Service.transport = sshTransport(t, fixture, fixture.Address)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("ssh-diagnostic", `{"steps":2,"step_delay_ms":50}`))
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
}

// TestIntegrationSSHTransportRecoversFromALostResponse proves that a response
// lost at the transport boundary is an explicit unknown outcome: the node task
// keeps running and the retry recovers through status without applying the
// effect twice.
func TestIntegrationSSHTransportRecoversFromALostResponse(t *testing.T) {
	pool, _ := testPool(t, true)
	fixture := startSSHFixture(t)
	proxy, err := sshfixture.NewCutProxy(fixture.Address, 50*time.Millisecond, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	run := testRuntime(t, pool, 2)
	run.Service.transport = sshTransport(t, fixture, proxy.Address())
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, submission("ssh-lost-response", `{"steps":1,"step_delay_ms":800}`))
	if err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	await(t, 90*time.Second, func() bool {
		current, err := run.Service.Get(ctx, testActor, op.ID)
		return err == nil && current.State == contract.Succeeded
	})
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
	if proxy.Cuts() == 0 {
		t.Fatal("the transport never lost a response")
	}
	current, _ := run.Service.Get(ctx, testActor, op.ID)
	if current.Attempt < 2 {
		t.Fatalf("the lost response did not cause a retry: attempt %d", current.Attempt)
	}
}
