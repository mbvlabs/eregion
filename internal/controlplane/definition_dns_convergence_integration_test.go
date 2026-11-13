package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const definitionTestZone = "0123456789abcdef0123456789abcdef"
const definitionTestHost = "app.example.com"

// This provider is used only behind a real PostgreSQL/application-service
// fixture. It deliberately puts sensitive text in failed response bodies.
type definitionDNSProviderFixture struct {
	mu             sync.Mutex
	records        map[string]integrations.DNSRecord
	status         int
	readStatus     int
	writes         int
	acceptWrite    bool
	disconnect     bool
	failAfterWrite bool
}

func (f *definitionDNSProviderFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.writes++
		if f.acceptWrite && r.Method == http.MethodPost {
			var record integrations.DNSRecord
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			record.ID = "created"
			f.records[record.ID] = record
			if f.failAfterWrite {
				f.readStatus = http.StatusForbidden
			}
		}
		if f.disconnect {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if f.status != 0 {
			http.Error(w, "private-provider-body token-and-ray-id", f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "created"}})
		return
	}
	if f.readStatus != 0 {
		http.Error(w, "private-provider-body token-and-ray-id", f.readStatus)
		return
	}
	records := make([]integrations.DNSRecord, 0, len(f.records))
	for _, record := range f.records {
		records = append(records, record)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "result": records, "result_info": map[string]int{"total_pages": 1},
	})
}

func definitionDNSFixture(t *testing.T, foreign bool, status int) (*pgxpool.Pool, *Service, api.AppService, *definitionDNSProviderFixture) {
	t.Helper()
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	s.routeSync = func(context.Context) error { return nil }
	fixture := &definitionDNSProviderFixture{records: map[string]integrations.DNSRecord{}, status: status}
	if foreign {
		fixture.records["foreign"] = integrations.DNSRecord{ID: "foreign", Name: definitionTestHost, Type: "A", Content: "8.8.8.8", TTL: 1, Comment: "external-owner"}
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	store := newTestDNSStore(integrations.DNSZone{ZoneID: definitionTestZone, ZoneName: "example.com"}).withToken(definitionTestZone, "ledger-token-0123456789")
	var err error
	s.domainDNS, err = integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: store, Credentials: store, BaseURL: server.URL + "/client/v4", HTTPClient: server.Client(),
		Authorizer: integrations.NewOperatorDNSAuthorizer(), Audit: integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
		(provider,zone_id,zone_name,latest_revision,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
		VALUES('cloudflare',$1,'example.com',1,'operator','test','operator','test',clock_timestamp(),clock_timestamp())`, definitionTestZone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_definition_dns_policy(singleton,revision,enabled,provider,zone_id,origins,proxy_mode,updated_at)
		VALUES(true,1,true,'cloudflare',$1,ARRAY['8.8.8.8'],'dns-only',clock_timestamp())
		ON CONFLICT(singleton) DO UPDATE SET revision=1,enabled=true,provider='cloudflare',zone_id=$1,origins=ARRAY['8.8.8.8'],proxy_mode='dns-only'`, definitionTestZone); err != nil {
		t.Fatal(err)
	}
	add := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/api/production", "initial-binding", map[string]any{"domain": definitionTestHost, "primary": true})
	if err := runPlatformAttempt(ctx, s, add.ID, 1); err != nil {
		t.Fatal(err)
	}
	return pool, s, app, fixture
}

func planAndSubmitDefinitionDNS(t *testing.T, app api.AppService, key string) contract.OperationID {
	t.Helper()
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "app.example.com"
primary = true
`}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: key, Confirm: "api/production",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Operation.ID
}

// Drive the real parent and each admitted child without starting host workers.
func driveDefinitionDNS(t *testing.T, s *Service, parent contract.OperationID) {
	t.Helper()
	ctx := context.Background()
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	worker := &platformWorker{service: s}
	for i := 0; i < 24; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		var state contract.State
		if err := s.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, parent).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state.Terminal() {
			return
		}
		var child *string
		if err := s.pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&child); err != nil {
			t.Fatal(err)
		}
		if child != nil {
			if err := runPlatformAttempt(ctx, s, contract.OperationID(*child), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("definition parent did not settle")
}

func TestIntegrationDefinitionDNSNoOpConvergence(t *testing.T) {
	pool, s, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	first := planAndSubmitDefinitionDNS(t, app, "first-dns-bookkeeping")
	driveDefinitionDNS(t, s, first)
	var generation, applied, revision int64
	var state, last string
	if err := pool.QueryRow(ctx, `SELECT x.generation,x.applied_generation,x.state,x.last_operation_id,d.desired_revision FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id WHERE d.name=$1`, definitionTestHost).Scan(&generation, &applied, &state, &last, &revision); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || applied != 1 || state != "applied" {
		t.Fatalf("first bookkeeping generation=%d applied=%d state=%s", generation, applied, state)
	}
	var marker string
	if err := pool.QueryRow(ctx, `SELECT ownership_marker FROM eregion_platform_domain_dns WHERE last_operation_id=$1`, last).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.records["owned"] = integrations.DNSRecord{ID: "owned", Name: definitionTestHost, Type: "A", Content: "8.8.8.8", TTL: 1, Comment: marker}
	provider.mu.Unlock()
	second := planAndSubmitDefinitionDNS(t, app, "repeat-dns-converged")
	var children int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_definition_steps WHERE parent_operation_id=$1 AND kind=$2`, second, platform.KindDomainAdd).Scan(&children); err != nil || children != 0 {
		t.Fatalf("repeat DNS steps=%d err=%v", children, err)
	}
	driveDefinitionDNS(t, s, second)
	var generationAfter, appliedAfter, revisionAfter int64
	var lastAfter string
	if err := pool.QueryRow(ctx, `SELECT x.generation,x.applied_generation,x.last_operation_id,d.desired_revision FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id WHERE d.name=$1`, definitionTestHost).Scan(&generationAfter, &appliedAfter, &lastAfter, &revisionAfter); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if generationAfter != generation || appliedAfter != applied || lastAfter != last || revisionAfter != revision || provider.writes != 0 || provider.records["foreign"].Comment != "external-owner" || provider.records["owned"].Comment != marker {
		t.Fatalf("repeat churn generation=%d applied=%d revision=%d writes=%d record=%+v", generationAfter, appliedAfter, revisionAfter, provider.writes, provider.records["foreign"])
	}
}

func TestIntegrationDefinitionDNSProviderDriftInvalidatesApproval(t *testing.T) {
	pool, _, app, provider := definitionDNSFixture(t, true, 0)
	ctx := context.Background()
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "app.example.com"
primary = true
`}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	provider.mu.Lock()
	record := provider.records["foreign"]
	record.Content = "1.1.1.1"
	provider.records["foreign"] = record
	provider.mu.Unlock()
	_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "foreign-drift", Confirm: "api/production",
	})
	fault := contract.Classify(err)
	if fault.Code != contract.Conflict || !strings.Contains(fault.Hint, "provider observations") {
		t.Fatalf("drift fault=%+v", fault)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_definition_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("stale provider approval left runs=%d err=%v", runs, err)
	}
}

func TestIntegrationDefinitionDNSChildPolicyCredentialDrift(t *testing.T) {
	for _, mutation := range []struct {
		name, query string
	}{
		{"policy", `UPDATE eregion_definition_dns_policy SET revision=revision+1 WHERE singleton=true`},
		{"credential", `UPDATE eregion_provider_credentials SET latest_revision=latest_revision+1 WHERE provider='cloudflare'`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			pool, s, app, provider := definitionDNSFixture(t, false, 0)
			ctx := context.Background()
			parent := planAndSubmitDefinitionDNS(t, app, "child-drift")
			claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
			if err != nil || !ok {
				t.Fatalf("claim=%v err=%v", ok, err)
			}
			worker := &platformWorker{service: s}
			var dnsChild string
			for i := 0; i < 12 && dnsChild == ""; i++ {
				err := worker.runDefinitionApply(ctx, claimed, 1)
				var snooze *river.JobSnoozeError
				if err != nil && !errors.As(err, &snooze) {
					t.Fatal(err)
				}
				var child, kind *string
				if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&child, &kind); err != nil {
					t.Fatal(err)
				}
				if child != nil && kind != nil {
					if *kind == platform.KindDomainAdd {
						dnsChild = *child
					} else if err := runPlatformAttempt(ctx, s, contract.OperationID(*child), 1); err != nil {
						t.Fatal(err)
					}
				}
			}
			if dnsChild == "" {
				t.Fatal("DNS child not admitted")
			}
			if _, err := pool.Exec(ctx, mutation.query); err != nil {
				t.Fatal(err)
			}
			if err := runPlatformAttempt(ctx, s, contract.OperationID(dnsChild), 1); err != nil {
				t.Fatal(err)
			}
			var state contract.State
			if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, dnsChild).Scan(&state); err != nil || state != contract.Failed {
				t.Fatalf("child state=%s err=%v", state, err)
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if provider.writes != 0 {
				t.Fatalf("drift allowed %d writes", provider.writes)
			}
		})
	}
}

func TestIntegrationDefinitionDNSProviderFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		status         int
		accepted       bool
		disconnect     bool
		failAfterWrite bool
	}{
		{http.StatusForbidden, false, false, false},
		{http.StatusTooManyRequests, false, false, false},
		{http.StatusInternalServerError, false, false, false},
		{http.StatusInternalServerError, true, false, false},
		{0, true, true, false},
		{0, true, false, true},
	} {
		t.Run(fmt.Sprintf("%d-accepted-%t-disconnect-%t-read403-%t", tc.status, tc.accepted, tc.disconnect, tc.failAfterWrite), func(t *testing.T) {
			status := tc.status
			pool, s, app, provider := definitionDNSFixture(t, false, status)
			provider.acceptWrite = tc.accepted
			provider.disconnect = tc.disconnect
			provider.failAfterWrite = tc.failAfterWrite
			ctx := context.Background()
			parent := planAndSubmitDefinitionDNS(t, app, "provider-failure")
			claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
			if err != nil || !ok {
				t.Fatalf("claim=%v err=%v", ok, err)
			}
			worker := &platformWorker{service: s}
			var dnsChild string
			for i := 0; i < 12 && dnsChild == ""; i++ {
				err := worker.runDefinitionApply(ctx, claimed, 1)
				var snooze *river.JobSnoozeError
				if err != nil && !errors.As(err, &snooze) {
					t.Fatal(err)
				}
				var child, kind *string
				if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&child, &kind); err != nil {
					t.Fatal(err)
				}
				if child != nil && kind != nil {
					if *kind == platform.KindDomainAdd {
						dnsChild = *child
					} else if err := runPlatformAttempt(ctx, s, contract.OperationID(*child), 1); err != nil {
						t.Fatal(err)
					}
				}
			}
			if dnsChild == "" {
				t.Fatal("DNS child not admitted")
			}
			runErr := runPlatformAttempt(ctx, s, contract.OperationID(dnsChild), 1)
			var state contract.State
			var message string
			if err := pool.QueryRow(ctx, `SELECT c.state,x.last_error FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id WHERE c.id=$1`, dnsChild).Scan(&state, &message); err != nil {
				t.Fatal(err)
			}
			expected := fmt.Sprintf("HTTP %d", status)
			if tc.disconnect {
				expected = "network/timeout"
			} else if tc.failAfterWrite {
				expected = "HTTP 403"
			}
			if !strings.Contains(message, expected) || strings.Contains(message, "private-provider-body") {
				t.Fatalf("unsafe provider diagnostic %q", message)
			}
			if status == http.StatusForbidden {
				if runErr != nil || state != contract.Failed {
					t.Fatalf("403 state=%s err=%v", state, runErr)
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
				var diagnosis string
				if err := pool.QueryRow(ctx, `SELECT failure_message FROM eregion_definition_runs WHERE operation_id=$1`, parent).Scan(&diagnosis); err != nil ||
					!strings.Contains(diagnosis, "authorization (HTTP 403)") || strings.Contains(diagnosis, "private-provider-body") || !strings.Contains(diagnosis, "serving, route, TLS and public") {
					t.Fatalf("parent diagnosis=%q err=%v", diagnosis, err)
				}
			} else if runErr == nil || state != contract.Running {
				t.Fatalf("retryable status %d state=%s err=%v", status, state, runErr)
			}
			if tc.accepted {
				provider.mu.Lock()
				provider.readStatus = http.StatusForbidden
				provider.mu.Unlock()
				if err := runPlatformAttempt(ctx, s, contract.OperationID(dnsChild), 2); contract.Classify(err).Code != contract.Unavailable {
					t.Fatalf("unresolved write must retry rejected read: %v", err)
				}
				var reserved bool
				if err := pool.QueryRow(ctx, `SELECT c.state,r.finished_at IS NULL FROM eregion_operations c JOIN eregion_definition_runs r ON r.operation_id=$2 WHERE c.id=$1`, dnsChild, parent).Scan(&state, &reserved); err != nil || state.Terminal() || !reserved {
					t.Fatalf("rejected read state=%s reserved=%v err=%v", state, reserved, err)
				}
				provider.mu.Lock()
				provider.readStatus = 0
				provider.mu.Unlock()
				if err := runPlatformAttempt(ctx, s, contract.OperationID(dnsChild), 3); err != nil {
					t.Fatalf("uncertain write readback: %v", err)
				}
				if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, dnsChild).Scan(&state); err != nil || state != contract.Succeeded {
					t.Fatalf("reconciled state=%s err=%v", state, err)
				}
			}
			if status == http.StatusTooManyRequests {
				if _, err := s.RequestCancel(ctx, definitionIntegrationActor, parent); err != nil {
					t.Fatal(err)
				}
				err := worker.runDefinitionApply(ctx, claimed, 1)
				var snooze *river.JobSnoozeError
				if !errors.As(err, &snooze) {
					t.Fatalf("parent did not wait for child: %v", err)
				}
				var parentState, childState contract.State
				var reserved bool
				if err := pool.QueryRow(ctx, `SELECT p.state,c.state,r.finished_at IS NULL FROM eregion_operations p JOIN eregion_definition_runs r ON r.operation_id=p.id JOIN eregion_operations c ON c.id=$2 WHERE p.id=$1`, parent, dnsChild).Scan(&parentState, &childState, &reserved); err != nil ||
					parentState != contract.Cancelling || childState != contract.Cancelling || !reserved {
					t.Fatalf("parent=%s child=%s reserved=%v err=%v", parentState, childState, reserved, err)
				}
			}
			provider.mu.Lock()
			writes := provider.writes
			provider.mu.Unlock()
			if writes != 1 {
				t.Fatalf("writes=%d", writes)
			}
		})
	}
}
