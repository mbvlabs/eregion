package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func TestIntegrationDefinitionDNSUnknownRecoveryRetainsReservation(t *testing.T) {
	for _, mode := range []string{"cancel", "exhaustion", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			testDefinitionDNSUnknownRecoveryMode(t, mode)
		})
	}
}

func testDefinitionDNSUnknownRecoveryMode(t *testing.T, mode string) {
	t.Helper()
	pool, s, app, provider := definitionDNSFixture(t, false, http.StatusInternalServerError)
	ctx := context.Background()
	parent := planAndSubmitDefinitionDNS(t, app, "recovery-"+mode)
	claimed := claimDefinitionParent(ctx, t, s, parent)
	child := string(admitDefinitionChild(ctx, t, pool, s, parent, claimed, platform.KindDomainAdd))
	worker := &platformWorker{service: s}
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("initial uncertain write: %v", err)
	}
	attempt := applyDefinitionDNSRecoveryMode(t, ctx, pool, s, worker, mode, parent, contract.OperationID(child), claimed)
	assertDefinitionDNSUnknownRecoveryRetained(t, ctx, pool, s, provider, parent, child, attempt)
	resolveDefinitionDNSUnknownRecovery(t, ctx, pool, s, provider, worker, parent, child, attempt, claimed)
}

func applyDefinitionDNSRecoveryMode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, worker *platformWorker, mode string, parent, child contract.OperationID, claimed contract.Operation) int {
	t.Helper()
	switch mode {
	case "cancel":
		if _, err := s.RequestCancel(ctx, definitionIntegrationActor, parent); err != nil {
			t.Fatal(err)
		}
		var snooze *river.JobSnoozeError
		if err := worker.runDefinitionApply(ctx, claimed, 1); !errors.As(err, &snooze) {
			t.Fatalf("drain: %v", err)
		}
	case "exhaustion":
		if _, err := pool.Exec(ctx, `UPDATE river_job SET state='discarded',finalized_at=clock_timestamp() WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, child); err != nil {
			t.Fatal(err)
		}
		op, _, err := s.operationSnapshot(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.reconcilePlatformOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
	case "orphan":
		var jobID int64
		if err := pool.QueryRow(ctx, `UPDATE river_job SET state='running',attempt=8,max_attempts=8,attempted_by=ARRAY['old-worker'],attempted_at=clock_timestamp() WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1) RETURNING id`, child).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		requeued, discarded, err := s.recoverOrphanedPlatformJob(ctx, orphanedPlatformJob{ID: jobID, OperationID: child, Attempt: 8, MaxAttempts: 8, LastClient: "old-worker"})
		if err != nil || !requeued || discarded {
			t.Fatalf("orphan requeued=%v discarded=%v err=%v", requeued, discarded, err)
		}
	}
	attempt := 2
	if mode == "orphan" {
		attempt = 9
	}
	return attempt
}

func assertDefinitionDNSUnknownRecoveryRetained(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, provider *definitionDNSProviderFixture, parent contract.OperationID, child string, attempt int) {
	t.Helper()
	var snooze *river.JobSnoozeError
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), attempt); !errors.As(err, &snooze) {
		t.Fatalf("pre-state must remain unknown: %v", err)
	}
	provider.mu.Lock()
	provider.readStatus = http.StatusForbidden
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), attempt+1); !errors.As(err, &snooze) {
		t.Fatalf("forbidden readback must retain reservation: %v", err)
	}
	var parentState, childState contract.State
	var reserved bool
	if err := pool.QueryRow(ctx, `SELECT p.state,c.state,r.finished_at IS NULL FROM eregion_operations p JOIN eregion_definition_runs r ON r.operation_id=p.id JOIN eregion_operations c ON c.id=$2 WHERE p.id=$1`, parent, child).Scan(&parentState, &childState, &reserved); err != nil || parentState.Terminal() || childState.Terminal() || !reserved {
		t.Fatalf("parent=%s child=%s reserved=%v err=%v", parentState, childState, reserved, err)
	}
}

func resolveDefinitionDNSUnknownRecovery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Service, provider *definitionDNSProviderFixture, worker *platformWorker, parent contract.OperationID, child string, attempt int, claimed contract.Operation) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT dns_plan->'desired' FROM eregion_definition_steps WHERE child_operation_id=$1`, child).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var desired []integrations.DNSRecord
	if err := json.Unmarshal(raw, &desired); err != nil || len(desired) != 1 {
		t.Fatalf("desired=%+v err=%v", desired, err)
	}
	desired[0].ID = "observed-late"
	provider.mu.Lock()
	provider.readStatus = 0
	provider.records[desired[0].ID] = desired[0]
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), attempt+2); err != nil {
		t.Fatalf("post-state readback: %v", err)
	}
	var childState contract.State
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&childState); err != nil || !childState.Terminal() {
		t.Fatalf("resolved child=%s err=%v", childState, err)
	}
	var parentState contract.State
	var reserved bool
	for i := 0; i < 4; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT o.state,r.finished_at IS NULL FROM eregion_operations o JOIN eregion_definition_runs r ON r.operation_id=o.id WHERE o.id=$1`, parent).Scan(&parentState, &reserved); err != nil {
			t.Fatal(err)
		}
		if parentState.Terminal() {
			break
		}
	}
	if !parentState.Terminal() || reserved {
		t.Fatalf("resolved parent=%s reserved=%v", parentState, reserved)
	}
	provider.mu.Lock()
	writes := provider.writes
	provider.mu.Unlock()
	if writes != 1 {
		t.Fatalf("recovery repeated provider writes: %d", writes)
	}
}

func TestIntegrationDefinitionDNSCleanupRecoveryIsReadOnly(t *testing.T) {
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	first := planAndSubmitDefinitionDNS(t, app, "cleanup-primary")
	driveDefinitionDNS(t, pool, s, first)
	provider.mu.Lock()
	provider.acceptWrite = true
	provider.mu.Unlock()
	add := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/api/production", "cleanup-secondary", map[string]any{
		"domain": "alias.example.com", "dns_mode": "cloudflare", "dns_zone": definitionTestZone,
		"dns_origins": []string{"8.8.8.8"}, "dns_proxied": false,
	})
	if err := runPlatformAttempt(ctx, s, add.ID, 1); err != nil {
		t.Fatal(err)
	}
	parent := planAndSubmitDefinitionDNS(t, app, "cleanup-parent")
	claimed := claimDefinitionParent(ctx, t, s, parent)
	child := string(admitDefinitionChild(ctx, t, pool, s, parent, claimed, platform.KindDomainRemove))
	worker := &platformWorker{service: s}
	provider.mu.Lock()
	provider.status = http.StatusInternalServerError
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("uncertain delete: %v", err)
	}
	provider.mu.Lock()
	_, exists := provider.records["created"]
	writesBeforeRecovery := provider.writes
	provider.mu.Unlock()
	if !exists {
		t.Fatal("fixture unexpectedly applied the uncertain delete")
	}
	var snooze *river.JobSnoozeError
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 2); !errors.As(err, &snooze) {
		t.Fatalf("uncertain delete must not be retried: %v", err)
	}
	provider.mu.Lock()
	writesAfterRecovery := provider.writes
	provider.mu.Unlock()
	if writesAfterRecovery != writesBeforeRecovery {
		t.Fatal("unknown cleanup recovery repeated a provider write")
	}
	if _, err := s.RequestCancel(ctx, definitionIntegrationActor, parent); err != nil {
		t.Fatal(err)
	}
	if err := worker.runDefinitionApply(ctx, claimed, 1); !errors.As(err, &snooze) {
		t.Fatalf("parent drain: %v", err)
	}
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 3); !errors.As(err, &snooze) {
		t.Fatalf("unresolved cleanup: %v", err)
	}
	var state contract.State
	var reserved bool
	if err := pool.QueryRow(ctx, `SELECT c.state,r.finished_at IS NULL FROM eregion_operations c JOIN eregion_definition_runs r ON r.operation_id=$2 WHERE c.id=$1`, child, parent).Scan(&state, &reserved); err != nil || state.Terminal() || !reserved {
		t.Fatalf("child=%s reserved=%v err=%v", state, reserved, err)
	}
	provider.mu.Lock()
	delete(provider.records, "created")
	writes := provider.writes
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 4); err != nil {
		t.Fatalf("resolved cleanup: %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.writes != writes || provider.records["foreign"].Comment != "external-owner" {
		t.Fatalf("recovery wrote provider state or changed foreign record: writes=%d records=%+v", provider.writes, provider.records)
	}
}

// An error response does not prove that the provider rejected the deletion.
// Readback of the exact absent owned record must settle without another write.
func TestIntegrationDefinitionDNSCleanupAcceptedErrorReadback(t *testing.T) {
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	first := planAndSubmitDefinitionDNS(t, app, "accepted-cleanup-primary")
	driveDefinitionDNS(t, pool, s, first)
	provider.mu.Lock()
	provider.acceptWrite = true
	provider.mu.Unlock()
	add := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/api/production", "accepted-cleanup-secondary", map[string]any{
		"domain": "alias.example.com", "dns_mode": "cloudflare", "dns_zone": definitionTestZone,
		"dns_origins": []string{"8.8.8.8"}, "dns_proxied": false,
	})
	if err := runPlatformAttempt(ctx, s, add.ID, 1); err != nil {
		t.Fatal(err)
	}
	parent := planAndSubmitDefinitionDNS(t, app, "accepted-cleanup-parent")
	claimed := claimDefinitionParent(ctx, t, s, parent)
	child := admitDefinitionChild(ctx, t, pool, s, parent, claimed, platform.KindDomainRemove)
	provider.mu.Lock()
	provider.acceptDelete = true
	provider.status = http.StatusInternalServerError
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, child, 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("uncertain accepted delete: %v", err)
	}
	provider.mu.Lock()
	_, exists := provider.records["created"]
	writes := provider.writes
	foreign := provider.records["foreign"]
	provider.mu.Unlock()
	if exists || foreign.Comment != "external-owner" {
		t.Fatalf("fixture did not apply only the owned delete: exists=%v foreign=%+v", exists, foreign)
	}
	if err := runPlatformAttempt(ctx, s, child, 2); err != nil {
		t.Fatalf("observed cleanup did not settle: %v", err)
	}
	var state contract.State
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil || state != contract.Failed {
		t.Fatalf("child state=%s err=%v", state, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.writes != writes || provider.records["foreign"] != foreign {
		t.Fatalf("readback repeated a write or changed foreign state: writes=%d records=%+v", provider.writes, provider.records)
	}
}
