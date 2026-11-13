package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func definitionPendingDNSChild(t *testing.T) (*Service, contract.Operation, contract.OperationID, *definitionDNSProviderFixture) {
	t.Helper()
	pool, s, app, provider := definitionDNSFixture(t, false, http.StatusInternalServerError)
	ctx := context.Background()
	parent := planAndSubmitDefinitionDNS(t, app, "review-pending")
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	worker := &platformWorker{service: s}
	for i := 0; i < 12; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		var id, kind *string
		if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		if id == nil || kind == nil {
			continue
		}
		if *kind == platform.KindDomainAdd {
			child := contract.OperationID(*id)
			if err := runPlatformAttempt(ctx, s, child, 1); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("pending write: %v", err)
			}
			return s, claimed, child, provider
		}
		if err := runPlatformAttempt(ctx, s, contract.OperationID(*id), 1); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("DNS child not admitted")
	return nil, contract.Operation{}, "", nil
}

func TestIntegrationDefinitionDNSMalformedJournalRetainsReservation(t *testing.T) {
	for _, scenario := range []string{"missing-plans", "missing-plans-result", "missing-link", "missing-cleanup-link"} {
		t.Run(scenario, func(t *testing.T) {
			s, parent, child, _ := definitionPendingDNSChild(t)
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, `UPDATE eregion_definition_steps SET dns_plan=NULL,dns_cleanup=NULL WHERE child_operation_id=$1`, child); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-plans-result" {
				if _, err := s.pool.Exec(ctx, `UPDATE eregion_definition_steps SET dns_progress=jsonb_set(dns_progress,'{0,result}','[]'::jsonb) WHERE child_operation_id=$1`, child); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.failPlatformOperation(ctx, child, 1, contract.Conflict, "fixture failure"); contract.Classify(err).Code != contract.Conflict {
				t.Fatalf("generic failure bypassed unresolved journal: %v", err)
			}
			if err := s.cancelPlatform(ctx, child, 1, "fixture cancellation"); contract.Classify(err).Code != contract.Conflict {
				t.Fatalf("generic cancellation bypassed unresolved journal: %v", err)
			}
			if _, err := s.pool.Exec(ctx, `UPDATE eregion_operations SET state='failed',error_code='unavailable' WHERE id=$1`, child); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-cleanup-link" {
				if _, err := s.pool.Exec(ctx, `UPDATE eregion_definition_steps SET dns_progress='[]'::jsonb,dns_cleanup='{"changes":[]}'::jsonb WHERE child_operation_id=$1`, child); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_domain_dns SET state='cleanup_pending' WHERE last_operation_id=$1`, child); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing-link" || scenario == "missing-cleanup-link" {
				if _, err := s.pool.Exec(ctx, `UPDATE eregion_definition_steps SET child_operation_id=NULL WHERE child_operation_id=$1`, child); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := guardDefinitionChildrenTerminalTx(ctx, tx, parent.ID); contract.Classify(err).Code != contract.Conflict {
				rollback(tx)
				t.Fatalf("parent terminal guard accepted broken journal: %v", err)
			}
			rollback(tx)
			if _, err := s.pool.Exec(ctx, `UPDATE eregion_operations SET state='failed',error_code='unavailable' WHERE id=$1`, parent.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.reconcileTerminalDefinitionRuns(ctx); err != nil {
				t.Fatal(err)
			}
			var reserved bool
			if err := s.pool.QueryRow(ctx, `SELECT finished_at IS NULL FROM eregion_definition_runs WHERE operation_id=$1`, parent.ID).Scan(&reserved); err != nil || !reserved {
				t.Fatalf("terminal-run repair dropped reservation=%v err=%v", reserved, err)
			}
		})
	}
}

func TestIntegrationDefinitionDNSMislinkedTerminalChildRetainsReservation(t *testing.T) {
	s, parent, child, _ := definitionPendingDNSChild(t)
	ctx := context.Background()
	var unrelated contract.OperationID
	if err := s.pool.QueryRow(ctx, `SELECT id FROM eregion_operations WHERE kind=$1 AND state='succeeded' ORDER BY id LIMIT 1`, platform.KindNodeRegister).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_definition_steps SET child_operation_id=$2 WHERE child_operation_id=$1`, child, unrelated); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", parent.ID))
	if err != nil {
		rollback(tx)
		t.Fatal(err)
	}
	if err := definitionFinishTx(ctx, tx, &locked, contract.Failed, contract.Conflict, "fixture"); contract.Classify(err).Code != contract.Conflict {
		rollback(tx)
		t.Fatalf("parent completion accepted mislinked DNS journal: %v", err)
	}
	rollback(tx)
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_operations SET state='failed',error_code='unavailable' WHERE id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileTerminalDefinitionRuns(ctx); err != nil {
		t.Fatal(err)
	}
	var reserved bool
	if err := s.pool.QueryRow(ctx, `SELECT finished_at IS NULL FROM eregion_definition_runs WHERE operation_id=$1`, parent.ID).Scan(&reserved); err != nil || !reserved {
		t.Fatalf("terminal repair dropped reservation=%v err=%v", reserved, err)
	}
}

func TestIntegrationDefinitionDNSRepeatedRecoveryClaimsAreBounded(t *testing.T) {
	s, parent, child, _ := definitionPendingDNSChild(t)
	ctx := context.Background()
	if _, err := s.RequestCancel(ctx, definitionIntegrationActor, parent.ID); err != nil {
		t.Fatal(err)
	}
	worker := &platformWorker{service: s}
	var snooze *river.JobSnoozeError
	if err := worker.runDefinitionApply(ctx, parent, 1); !errors.As(err, &snooze) {
		t.Fatalf("initial drain: %v", err)
	}
	for attempt := 2; attempt <= 5; attempt++ {
		if err := runPlatformAttempt(ctx, s, child, attempt); !errors.As(err, &snooze) {
			t.Fatalf("recovery attempt %d: %v", attempt, err)
		}
		if err := runPlatformAttempt(ctx, s, parent.ID, attempt); !errors.As(err, &snooze) {
			t.Fatalf("parent attempt %d: %v", attempt, err)
		}
	}
	var childClaims, parentClaims, warnings int
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='platform'),
		(SELECT count(*) FROM eregion_operation_events WHERE operation_id=$2 AND event->>'stage'='platform'),
		(SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'='dns_outcome_unknown')`, child, parent.ID).Scan(&childClaims, &parentClaims, &warnings); err != nil {
		t.Fatal(err)
	}
	if childClaims != 1 || parentClaims != 1 || warnings != 1 {
		t.Fatalf("child claims=%d parent claims=%d warnings=%d", childClaims, parentClaims, warnings)
	}
}

func TestIntegrationDefinitionManualDomainWithoutDNSJournalSettles(t *testing.T) {
	_, s, app := definitionIntegrationFixture(t)
	s.routeSync = func(context.Context) error { return nil }
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "manual.example.com"
primary = true
`}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{Plan: input, ApprovedDigest: plan.Digest, Confirm: "api/production", IdempotencyKey: "manual-domain"})
	if err != nil {
		t.Fatal(err)
	}
	driveDefinitionDNS(t, s, result.Operation.ID)
	op, _, err := s.operationSnapshot(ctx, result.Operation.ID)
	if err != nil || op.State != contract.Succeeded || strings.Contains(string(op.ErrorCode), "conflict") {
		t.Fatalf("manual definition state=%s err=%v", op.State, err)
	}
}

func TestIntegrationDefinitionDNSCleanupDefinitive403(t *testing.T) {
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	first := planAndSubmitDefinitionDNS(t, app, "reject-primary")
	driveDefinitionDNS(t, s, first)
	provider.mu.Lock()
	provider.acceptWrite = true
	provider.mu.Unlock()
	add := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/api/production", "reject-secondary", map[string]any{
		"domain": "alias.example.com", "dns_mode": "cloudflare", "dns_zone": definitionTestZone,
		"dns_origins": []string{"8.8.8.8"}, "dns_proxied": false,
	})
	if err := runPlatformAttempt(ctx, s, add.ID, 1); err != nil {
		t.Fatal(err)
	}
	parent := planAndSubmitDefinitionDNS(t, app, "reject-cleanup")
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	worker := &platformWorker{service: s}
	var child string
	for i := 0; i < 12 && child == ""; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		var id, kind *string
		if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		if id != nil && kind != nil {
			if *kind == platform.KindDomainRemove {
				child = *id
			} else if err := runPlatformAttempt(ctx, s, contract.OperationID(*id), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if child == "" {
		t.Fatal("cleanup child not admitted")
	}
	provider.mu.Lock()
	provider.status = http.StatusForbidden
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 1); err != nil {
		t.Fatalf("deterministic rejection should stop: %v", err)
	}
	var state contract.State
	var cleanupError string
	if err := pool.QueryRow(ctx, `SELECT c.state,x.cleanup_error FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id WHERE c.id=$1`, child).Scan(&state, &cleanupError); err != nil || state != contract.Failed || !strings.HasPrefix(cleanupError, "definitive cleanup rejection: ") || !strings.Contains(cleanupError, "authorization (HTTP 403)") || strings.Contains(cleanupError, "private-provider-body") {
		t.Fatalf("cleanup state=%s error=%q err=%v", state, cleanupError, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domain_dns SET cleanup_error=$2 WHERE last_operation_id=$1`, child, "definitive cleanup rejection: "+strings.Repeat("0", 64)+" Cloudflare provider: authorization (HTTP 403)"); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := guardDefinitionDNSTerminalTx(ctx, tx, &contract.Operation{ID: contract.OperationID(child), Kind: platform.KindDomainRemove, State: state}); err == nil {
		rollback(tx)
		t.Fatal("cleanup rejection with a mismatched record digest was accepted")
	}
	rollback(tx)
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domain_dns SET cleanup_error=$2 WHERE last_operation_id=$1`, child, cleanupError); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		if err == nil {
			break
		}
	}
	var message string
	if err := pool.QueryRow(ctx, `SELECT failure_message FROM eregion_definition_runs WHERE operation_id=$1`, parent).Scan(&message); err != nil || !strings.Contains(message, "authorization (HTTP 403)") || strings.Contains(message, "private-provider-body") {
		t.Fatalf("parent message=%q err=%v", message, err)
	}
}

func TestIntegrationDefinitionDNSCleanupRecoveredFailureRetainsDiagnostic(t *testing.T) {
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	first := planAndSubmitDefinitionDNS(t, app, "prior-primary")
	driveDefinitionDNS(t, s, first)
	provider.mu.Lock()
	provider.acceptWrite = true
	provider.mu.Unlock()
	add := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/api/production", "prior-secondary", map[string]any{
		"domain": "alias.example.com", "dns_mode": "cloudflare", "dns_zone": definitionTestZone,
		"dns_origins": []string{"8.8.8.8"}, "dns_proxied": false,
	})
	if err := runPlatformAttempt(ctx, s, add.ID, 1); err != nil {
		t.Fatal(err)
	}
	parent := planAndSubmitDefinitionDNS(t, app, "prior-cleanup")
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	worker := &platformWorker{service: s}
	var child string
	for i := 0; i < 12 && child == ""; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		var id, kind *string
		if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		if id != nil && kind != nil {
			if *kind == platform.KindDomainRemove {
				child = *id
			} else if err := runPlatformAttempt(ctx, s, contract.OperationID(*id), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if child == "" {
		t.Fatal("cleanup child not admitted")
	}
	provider.mu.Lock()
	provider.status = http.StatusInternalServerError
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 1); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("uncertain delete: %v", err)
	}
	var snooze *river.JobSnoozeError
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 2); !errors.As(err, &snooze) {
		t.Fatalf("unresolved cleanup: %v", err)
	}
	provider.mu.Lock()
	delete(provider.records, "created")
	writes := provider.writes
	provider.mu.Unlock()
	if err := runPlatformAttempt(ctx, s, contract.OperationID(child), 3); err != nil {
		t.Fatalf("read-only cleanup recovery: %v", err)
	}
	var childState contract.State
	var ledgerState, cleanupError string
	var diagnostics int
	if err := pool.QueryRow(ctx, `SELECT c.state,x.state,x.cleanup_error,(SELECT count(*) FROM eregion_operation_events WHERE operation_id=c.id AND event->>'stage'='dns_prior_provider_failure') FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id WHERE c.id=$1`, child).Scan(&childState, &ledgerState, &cleanupError, &diagnostics); err != nil ||
		childState != contract.Failed || ledgerState != domainDNSStateCleanupVerified || cleanupError != "" || diagnostics != 1 {
		t.Fatalf("child=%s ledger=%s error=%q diagnostics=%d err=%v", childState, ledgerState, cleanupError, diagnostics, err)
	}
	for i := 0; i < 3; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		if err == nil {
			break
		}
		if !errors.As(err, &snooze) {
			t.Fatal(err)
		}
	}
	var message string
	if err := pool.QueryRow(ctx, `SELECT failure_message FROM eregion_definition_runs WHERE operation_id=$1`, parent).Scan(&message); err != nil || !strings.Contains(message, "Prior provider diagnostic: Cloudflare provider: provider-unavailable (HTTP 500)") || strings.Contains(message, "private-provider-body") {
		t.Fatalf("parent diagnosis=%q err=%v", message, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.writes != writes {
		t.Fatalf("recovery made another write: %d -> %d", writes, provider.writes)
	}
}
