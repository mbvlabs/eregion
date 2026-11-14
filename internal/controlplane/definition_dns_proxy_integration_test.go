package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func proxyDefinitionInput() api.DefinitionPlanInput {
	return api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "app.example.com"
primary = true
`}
}

func proxyFixture(t *testing.T) (*Service, api.AppService, *definitionDNSProviderFixture) {
	t.Helper()
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	if _, err := pool.Exec(context.Background(), `UPDATE eregion_definition_dns_policy SET proxy_mode='proxied' WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	record := provider.records["foreign"]
	record.TTL = 300
	provider.records["foreign"] = record
	return s, app, provider
}

func proxyChild(t *testing.T, s *Service, parent contract.OperationID) contract.OperationID {
	t.Helper()
	ctx := context.Background()
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
		if err := s.pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		if id != nil && kind != nil {
			if *kind == platform.KindDomainAdd {
				return contract.OperationID(*id)
			}
			if err := runPlatformAttempt(ctx, s, contract.OperationID(*id), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("DNS child not admitted")
	return ""
}

func TestIntegrationDefinitionDNSProxyTransfer(t *testing.T) {
	for _, scenario := range []string{"success", "rejected", "lost-response"} {
		t.Run(scenario, func(t *testing.T) {
			s, app, provider := proxyFixture(t)
			ctx := context.Background()
			input := proxyDefinitionInput()
			plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
			if err != nil || !plan.CanApply || len(plan.DNS) != 1 || len(plan.DNS[0].Changes) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			dns := plan.DNS[0]
			change := dns.Changes[0]
			if !dns.RequiresApproval || change.Action != "update" || change.Before.ID != "foreign" || change.Before.Ownership != "foreign" || change.Before.TTL != 300 || change.After.ID != "foreign" || change.After.Comment != dns.Ownership || !change.After.Proxied || change.After.TTL != 1 || !strings.Contains(strings.Join(dns.Warnings, " "), "automatic TTL") {
				t.Fatalf("unexpected transfer plan: %+v", dns)
			}
			input.PlanNonce = plan.PlanNonce
			result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "proxy-transfer", Confirm: "api/production"})
			if err != nil {
				t.Fatal(err)
			}
			child := proxyChild(t, s, result.Operation.ID)
			provider.mu.Lock()
			provider.acceptWrite = scenario != "rejected"
			provider.disconnect = scenario == "lost-response"
			if scenario == "rejected" {
				provider.status = http.StatusBadRequest
			}
			provider.mu.Unlock()
			err = runPlatformAttempt(ctx, s, child, 1)
			if scenario == "lost-response" {
				if contract.Classify(err).Code != contract.Unavailable {
					t.Fatalf("lost response: %v", err)
				}
				provider.mu.Lock()
				provider.disconnect = false
				provider.mu.Unlock()
				err = runPlatformAttempt(ctx, s, child, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			var state contract.State
			if err := s.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil {
				t.Fatal(err)
			}
			wantState := contract.Succeeded
			if scenario == "rejected" {
				wantState = contract.Failed
			}
			if state != wantState {
				t.Fatalf("state=%s want=%s", state, wantState)
			}
			provider.mu.Lock()
			record := provider.records["foreign"]
			if !reflect.DeepEqual(provider.methods, []string{http.MethodPatch}) || len(provider.records) != 1 {
				provider.mu.Unlock()
				t.Fatalf("methods=%v records=%+v", provider.methods, provider.records)
			}
			if scenario == "rejected" {
				if record.Proxied || record.TTL != 300 || record.Comment != "external-owner" {
					provider.mu.Unlock()
					t.Fatalf("rejection changed record: %+v", record)
				}
			} else if !record.Proxied || record.TTL != 1 || record.Comment != dns.Ownership || len(provider.patchBody) != 3 || provider.patchBody["comment"] != dns.Ownership || provider.patchBody["proxied"] != true || provider.patchBody["ttl"] != float64(1) {
				provider.mu.Unlock()
				t.Fatalf("record=%+v patch=%v", record, provider.patchBody)
			}
			provider.mu.Unlock()
			if scenario != "rejected" {
				cleanup, err := s.domainDNS.PlanRemove(ctx, integrations.DomainDNSRequest{ZoneID: dns.ZoneID, ZoneName: dns.Zone, DomainID: dns.DomainID, Ownership: dns.Ownership, Host: dns.Host})
				if err != nil || len(cleanup.Changes) != 1 || cleanup.Changes[0].Before.ID != "foreign" || cleanup.Changes[0].Before.Ownership != "owned" {
					t.Fatalf("cleanup=%+v err=%v", cleanup, err)
				}
			}
		})
	}
}

func TestIntegrationDefinitionDNSProxySnapshotAndAmbiguity(t *testing.T) {
	for _, scenario := range []string{"id", "comment", "content", "proxy", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			s, app, provider := proxyFixture(t)
			ctx := context.Background()
			input := proxyDefinitionInput()
			plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
			if err != nil || !plan.CanApply {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			input.PlanNonce = plan.PlanNonce
			provider.mu.Lock()
			r := provider.records["foreign"]
			switch scenario {
			case "id":
				delete(provider.records, "foreign")
				r.ID = "different"
			case "comment":
				r.Comment = "changed"
			case "content":
				r.Content = "1.1.1.1"
			case "proxy":
				r.Proxied = true
			case "duplicate":
				duplicate := r
				duplicate.ID = "duplicate"
				provider.records[duplicate.ID] = duplicate
			}
			provider.records[r.ID] = r
			provider.mu.Unlock()
			_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "proxy-drift", Confirm: "api/production"})
			if err == nil {
				t.Fatal("stale approval accepted")
			}
			if scenario == "duplicate" {
				newPlan, planErr := app.PlanDefinition(ctx, definitionIntegrationActor, proxyDefinitionInput())
				if planErr == nil && newPlan.CanApply {
					t.Fatalf("ambiguous plan accepted: %+v", newPlan)
				}
			}
			var runs int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_definition_runs`).Scan(&runs); err != nil || runs != 0 || provider.writes != 0 {
				t.Fatalf("runs=%d writes=%d err=%v", runs, provider.writes, err)
			}
		})
	}
}

func TestIntegrationDefinitionDNSProxyExecutionDrift(t *testing.T) {
	for _, field := range []string{"id", "comment", "content", "proxy", "ttl"} {
		t.Run(field, func(t *testing.T) {
			s, app, provider := proxyFixture(t)
			ctx := context.Background()
			parent := planAndSubmitDefinitionDNS(t, app, "proxy-execution-drift")
			child := proxyChild(t, s, parent)
			provider.mu.Lock()
			record := provider.records["foreign"]
			switch field {
			case "id":
				delete(provider.records, record.ID)
				record.ID = "different"
			case "comment":
				record.Comment = "other-owner"
			case "content":
				record.Content = "1.1.1.1"
			case "proxy":
				record.Proxied = true
			case "ttl":
				record.TTL = 600
			}
			provider.records[record.ID] = record
			provider.mu.Unlock()
			_ = runPlatformAttempt(ctx, s, child, 1)
			var state contract.State
			if err := s.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil || state == contract.Succeeded {
				t.Fatalf("state=%s err=%v", state, err)
			}
			provider.mu.Lock()
			writes := provider.writes
			provider.mu.Unlock()
			if writes != 0 {
				t.Fatalf("drift permitted %d writes", writes)
			}
		})
	}
}

func TestIntegrationDefinitionDNSProxyReadbackMismatch(t *testing.T) {
	for _, field := range []string{"id", "proxy", "marker"} {
		t.Run(field, func(t *testing.T) {
			s, app, provider := proxyFixture(t)
			ctx := context.Background()
			parent := planAndSubmitDefinitionDNS(t, app, "proxy-readback-mismatch")
			child := proxyChild(t, s, parent)
			provider.mu.Lock()
			provider.acceptWrite = true
			provider.patchMutation = field
			provider.mu.Unlock()
			_ = runPlatformAttempt(ctx, s, child, 1)
			var state contract.State
			var reserved, observed bool
			if err := s.pool.QueryRow(ctx, `SELECT c.state,r.finished_at IS NULL,
				coalesce(s.dns_progress->0->'result' IS NOT NULL AND s.dns_progress->0->'result' <> 'null'::jsonb,false)
				FROM eregion_definition_steps s JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
				JOIN eregion_operations c ON c.id=s.child_operation_id WHERE c.id=$1`, child).Scan(&state, &reserved, &observed); err != nil {
				t.Fatal(err)
			}
			provider.mu.Lock()
			methods := append([]string(nil), provider.methods...)
			provider.mu.Unlock()
			if state == contract.Succeeded || !reserved || observed || !reflect.DeepEqual(methods, []string{http.MethodPatch}) {
				t.Fatalf("state=%s reserved=%v observed=%v methods=%v", state, reserved, observed, methods)
			}
		})
	}
}

func TestIntegrationDefinitionDNSProxyRecoveryFence(t *testing.T) {
	for _, mode := range []string{"cancel", "exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			s, app, provider := proxyFixture(t)
			ctx := context.Background()
			parent := planAndSubmitDefinitionDNS(t, app, "proxy-recovery")
			child := proxyChild(t, s, parent)
			provider.mu.Lock()
			provider.status = http.StatusInternalServerError
			provider.mu.Unlock()
			if err := runPlatformAttempt(ctx, s, child, 1); contract.Classify(err).Code != contract.Unavailable {
				t.Fatalf("uncertain PATCH: %v", err)
			}
			switch mode {
			case "cancel":
				if _, err := s.RequestCancel(ctx, definitionIntegrationActor, parent); err != nil {
					t.Fatal(err)
				}
				op, _, err := s.operationSnapshot(ctx, parent)
				if err != nil {
					t.Fatal(err)
				}
				var snooze *river.JobSnoozeError
				if err := (&platformWorker{service: s}).runDefinitionApply(ctx, op, 1); !errors.As(err, &snooze) {
					t.Fatalf("cancel drain: %v", err)
				}
			case "exhaustion":
				if _, err := s.pool.Exec(ctx, `UPDATE river_job SET state='discarded',finalized_at=clock_timestamp() WHERE id=(SELECT job_id FROM eregion_operations WHERE id=$1)`, child); err != nil {
					t.Fatal(err)
				}
				op, _, err := s.operationSnapshot(ctx, child)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.reconcilePlatformOperation(ctx, op); err != nil {
					t.Fatal(err)
				}
			}
			var snooze *river.JobSnoozeError
			if err := runPlatformAttempt(ctx, s, child, 2); !errors.As(err, &snooze) {
				t.Fatalf("pre-state recovery must remain uncertain: %v", err)
			}
			var state contract.State
			var reserved bool
			var raw []byte
			if err := s.pool.QueryRow(ctx, `SELECT c.state,r.finished_at IS NULL,s.dns_plan
				FROM eregion_definition_steps s JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
				JOIN eregion_operations c ON c.id=s.child_operation_id WHERE c.id=$1`, child).Scan(&state, &reserved, &raw); err != nil || state.Terminal() || !reserved {
				t.Fatalf("state=%s reserved=%v err=%v", state, reserved, err)
			}
			var approved integrations.DefinitionDNSPlan
			if err := json.Unmarshal(raw, &approved); err != nil || len(approved.Changes) != 1 || approved.Changes[0].After == nil {
				t.Fatalf("invalid approved plan: %v", err)
			}
			// Model a delayed accepted PATCH, then reconcile only by readback.
			provider.mu.Lock()
			provider.records["foreign"] = *approved.Changes[0].After
			provider.mu.Unlock()
			if err := runPlatformAttempt(ctx, s, child, 3); err != nil {
				t.Fatalf("post-state recovery: %v", err)
			}
			if err := s.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil || !state.Terminal() {
				t.Fatalf("state=%s err=%v", state, err)
			}
			provider.mu.Lock()
			methods := append([]string(nil), provider.methods...)
			provider.mu.Unlock()
			if !reflect.DeepEqual(methods, []string{http.MethodPatch}) {
				t.Fatalf("recovery repeated writes: %v", methods)
			}
		})
	}
}

func TestIntegrationDefinitionDNSMatchingForeignRemainsUnowned(t *testing.T) {
	s, app, provider := proxyFixture(t)
	provider.records["foreign"] = integrations.DNSRecord{ID: "foreign", Name: definitionTestHost, Type: "A", Content: "8.8.8.8", TTL: 1, Proxied: true, Comment: "external-owner"}
	plan, err := app.PlanDefinition(context.Background(), definitionIntegrationActor, proxyDefinitionInput())
	if err != nil || !plan.CanApply || len(plan.DNS) != 1 || len(plan.DNS[0].Changes) != 0 || len(plan.DNS[0].ExternalSatisfaction) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	parent := planAndSubmitDefinitionDNS(t, app, "matching-foreign")
	driveDefinitionDNS(t, s, parent)
	if provider.writes != 0 || provider.records["foreign"].Comment != "external-owner" {
		t.Fatalf("foreign record changed: %+v", provider.records)
	}
}
