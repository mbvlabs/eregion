package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	errorBody      string
	echoRequest    bool
	readStatus     int
	writes         int
	acceptWrite    bool
	acceptDelete   bool
	disconnect     bool
	failAfterWrite bool
	methods        []string
	patchBody      map[string]any
	patchMutation  string
}

func (f *definitionDNSProviderFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		f.serveRead(w)
		return
	}
	f.writes++
	f.methods = append(f.methods, r.Method)
	if f.acceptWrite && r.Method == http.MethodPost {
		if !f.createRecord(w, r) {
			return
		}
	}
	if f.acceptWrite && r.Method == http.MethodPatch {
		if !f.patchRecord(w, r) {
			return
		}
	}
	// Deletion is independent of create/PATCH acceptance: tests opt in to
	// an applied delete even when its response reports an error.
	if f.acceptDelete && r.Method == http.MethodDelete {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		delete(f.records, id)
	}
	if f.disconnect {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
		return
	}
	if f.status != 0 {
		f.writeError(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "created"}})
}

func (f *definitionDNSProviderFixture) createRecord(w http.ResponseWriter, r *http.Request) bool {
	var record integrations.DNSRecord
	if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return false
	}
	record.ID = "created"
	f.records[record.ID] = record
	if f.failAfterWrite {
		f.readStatus = http.StatusForbidden
	}
	return true
}

func (f *definitionDNSProviderFixture) patchRecord(w http.ResponseWriter, r *http.Request) bool {
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return false
	}
	f.patchBody = body
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	record, ok := f.records[id]
	if !ok {
		http.NotFound(w, r)
		return false
	}
	record.Proxied, _ = body["proxied"].(bool)
	record.Comment, _ = body["comment"].(string)
	if record.Proxied {
		record.TTL = 1 // Cloudflare forces automatic TTL when proxied.
	} else if ttl, ok := body["ttl"].(float64); ok {
		record.TTL = int(ttl)
	}
	switch f.patchMutation {
	case "id":
		delete(f.records, id)
		record.ID = "different"
	case "proxy":
		record.Proxied = false
	case "marker":
		record.Comment = "other-owner"
	}
	f.records[record.ID] = record
	return true
}

func (f *definitionDNSProviderFixture) writeError(w http.ResponseWriter, r *http.Request) {
	body := f.errorBody
	if f.echoRequest {
		requestBody, _ := io.ReadAll(r.Body)
		raw, _ := json.Marshal(map[string]any{
			"errors": []any{map[string]any{"code": 81053, "message": r.Header.Get("Authorization")}},
			"result": map[string]string{"url": r.URL.String(), "request": string(requestBody)},
		})
		body = string(raw)
	}
	if body == "" {
		body = "private-provider-body token-and-ray-id"
	}
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(body))
}

func (f *definitionDNSProviderFixture) serveRead(w http.ResponseWriter) {
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
	insertTestProviderCredential(t, pool, definitionTestZone, "example.com")
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

func claimDefinitionParent(ctx context.Context, t *testing.T, s *Service, parent contract.OperationID) contract.Operation {
	t.Helper()
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	return claimed
}

func queuedDefinitionChild(ctx context.Context, t *testing.T, pool *pgxpool.Pool, parent contract.OperationID) (contract.OperationID, string, bool) {
	t.Helper()
	var id, kind *string
	if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1), (SELECT s.kind FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&id, &kind); err != nil {
		t.Fatal(err)
	}
	if id == nil || kind == nil {
		return "", "", false
	}
	return contract.OperationID(*id), *kind, true
}

func admitDefinitionChild(ctx context.Context, t *testing.T, pool *pgxpool.Pool, s *Service, parent contract.OperationID, claimed contract.Operation, wantKind string) contract.OperationID {
	t.Helper()
	worker := &platformWorker{service: s}
	for i := 0; i < 12; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		child, kind, found := queuedDefinitionChild(ctx, t, pool, parent)
		if !found {
			continue
		}
		if kind == wantKind {
			return child
		}
		if err := runPlatformAttempt(ctx, s, child, 1); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("definition child %s not admitted", wantKind)
	return ""
}

// Drive the real parent and each admitted child without starting host workers.
func driveDefinitionDNS(t *testing.T, pool *pgxpool.Pool, s *Service, parent contract.OperationID) {
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
		if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, parent).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state.Terminal() {
			return
		}
		var child *string
		if err := pool.QueryRow(ctx, `SELECT (SELECT s.child_operation_id FROM eregion_definition_steps s JOIN eregion_operations c ON c.id=s.child_operation_id WHERE s.parent_operation_id=$1 AND c.state='queued' ORDER BY s.ordinal LIMIT 1)`, parent).Scan(&child); err != nil {
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
	driveDefinitionDNS(t, pool, s, first)
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
	driveDefinitionDNS(t, pool, s, second)
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
			claimed := claimDefinitionParent(ctx, t, s, parent)
			dnsChild := string(admitDefinitionChild(ctx, t, pool, s, parent, claimed, platform.KindDomainAdd))
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

type definitionDNSFailureCase struct {
	name           string
	status         int
	accepted       bool
	disconnect     bool
	failAfterWrite bool
	body           string
	detail         string
	echo           bool
}

type definitionDNSFailureRun struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	service  *Service
	provider *definitionDNSProviderFixture
	parent   contract.OperationID
	claimed  contract.Operation
	child    contract.OperationID
	worker   *platformWorker
	runErr   error
	state    contract.State
	message  string
}

func TestIntegrationDefinitionDNSProviderFailureClassification(t *testing.T) {
	for _, tc := range []definitionDNSFailureCase{
		{name: "known", status: 400, body: `{"errors":[{"code":81053,"message":"ledger-token-0123456789 private-provider-body"}],"result":{"secret":"ledger-token-0123456789"}}`, detail: "(code 81053: a DNS record with these details already exists)"},
		{name: "echo", status: 400, echo: true, detail: "(code 81053: a DNS record with these details already exists)"},
		{name: "unknown", status: 400, body: `{"errors":[{"code":999123,"message":"private-provider-body"}]}`, detail: "(code 999123: provider error; inspect provider configuration)"},
		{name: "malformed", status: 400, body: `{"errors":[{"code":81053`, detail: ""},
		{name: "oversized", status: 400, body: `{"errors":[{"code":81053}],"message":"` + strings.Repeat("x", (8<<20)+1) + `"}`, detail: ""},
		{name: "string-code", status: 400, body: `{"errors":[{"code":"81053","message":"private-provider-body"}]}`, detail: ""},
		{name: "fraction-code", status: 400, body: `{"errors":[{"code":81053.0}]}`},
		{name: "out-of-range-code", status: 400, body: `{"errors":[{"code":1000000000}]}`},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "timeout", status: http.StatusRequestTimeout},
		{name: "rate-limit", status: http.StatusTooManyRequests},
		{name: "server-error", status: http.StatusInternalServerError},
		{name: "accepted-server-error", status: http.StatusInternalServerError, accepted: true},
		{name: "disconnect", accepted: true, disconnect: true},
		{name: "read-forbidden", accepted: true, failAfterWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDefinitionDNSProviderFailureScenario(t, tc)
		})
	}
}

func testDefinitionDNSProviderFailureScenario(t *testing.T, tc definitionDNSFailureCase) {
	t.Helper()
	run := newDefinitionDNSFailureRun(t, tc)
	expected := fmt.Sprintf("HTTP %d", tc.status)
	if tc.disconnect {
		expected = "network/timeout"
	} else if tc.failAfterWrite {
		expected = "HTTP 403"
	}
	assertDefinitionDNSFailureDiagnostic(t, run.message, expected, tc)
	if tc.status == http.StatusForbidden || tc.status == http.StatusUnauthorized || tc.status == http.StatusBadRequest {
		assertDefinitionDNSFailureTerminal(t, run, tc, expected)
	} else if run.runErr == nil || run.state != contract.Running {
		t.Fatalf("retryable status %d state=%s err=%v", tc.status, run.state, run.runErr)
	}
	if tc.accepted {
		assertDefinitionDNSAcceptedRecovery(t, run)
	}
	if tc.status == http.StatusTooManyRequests {
		assertDefinitionDNSRateLimitCancellation(t, run)
	}
	assertDefinitionDNSFailureWrites(t, run)
}

func newDefinitionDNSFailureRun(t *testing.T, tc definitionDNSFailureCase) definitionDNSFailureRun {
	t.Helper()
	pool, s, app, provider := definitionDNSFixture(t, false, tc.status)
	provider.acceptWrite = tc.accepted
	provider.disconnect = tc.disconnect
	provider.failAfterWrite = tc.failAfterWrite
	provider.errorBody = tc.body
	provider.echoRequest = tc.echo
	ctx := context.Background()
	parent := planAndSubmitDefinitionDNS(t, app, "provider-failure")
	claimed := claimDefinitionParent(ctx, t, s, parent)
	child := admitDefinitionChild(ctx, t, pool, s, parent, claimed, platform.KindDomainAdd)
	run := definitionDNSFailureRun{
		ctx: ctx, pool: pool, service: s, provider: provider,
		parent: parent, claimed: claimed, child: child, worker: &platformWorker{service: s},
	}
	run.runErr = runPlatformAttempt(ctx, s, child, 1)
	if err := pool.QueryRow(ctx, `SELECT c.state,x.last_error FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id WHERE c.id=$1`, child).Scan(&run.state, &run.message); err != nil {
		t.Fatal(err)
	}
	return run
}

func assertDefinitionDNSFailureDiagnostic(t *testing.T, message, expected string, tc definitionDNSFailureCase) {
	t.Helper()
	if !strings.Contains(message, expected) || strings.Contains(message, "private-provider-body") {
		t.Fatalf("unsafe provider diagnostic %q", message)
	}
	if tc.detail != "" && !strings.Contains(message, tc.detail) || tc.detail == "" && strings.Contains(message, "(code ") || strings.Contains(message, "ledger-token-0123456789") {
		t.Fatalf("unexpected safe detail %q", message)
	}
}

func assertDefinitionDNSFailureTerminal(t *testing.T, run definitionDNSFailureRun, tc definitionDNSFailureCase, expected string) {
	t.Helper()
	if run.runErr != nil || run.state != contract.Failed {
		t.Fatalf("403 state=%s err=%v", run.state, run.runErr)
	}
	for i := 0; i < 3; i++ {
		err := run.worker.runDefinitionApply(run.ctx, run.claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatal(err)
		}
		if err == nil {
			break
		}
	}
	var diagnosis string
	if err := run.pool.QueryRow(run.ctx, `SELECT failure_message FROM eregion_definition_runs WHERE operation_id=$1`, run.parent).Scan(&diagnosis); err != nil ||
		!strings.Contains(diagnosis, expected) || tc.detail != "" && !strings.Contains(diagnosis, tc.detail) || strings.Contains(diagnosis, "private-provider-body") || strings.Contains(diagnosis, "ledger-token-0123456789") || !strings.Contains(diagnosis, "serving, route, TLS and public") {
		t.Fatalf("parent diagnosis=%q err=%v", diagnosis, err)
	}
	var events string
	if err := run.pool.QueryRow(run.ctx, `SELECT coalesce(string_agg(event::text, ' '),'') FROM eregion_operation_events WHERE operation_id IN ($1,$2)`, run.parent, run.child).Scan(&events); err != nil ||
		strings.Contains(events, "private-provider-body") || strings.Contains(events, "ledger-token-0123456789") || tc.detail != "" && !strings.Contains(events, tc.detail) {
		t.Fatalf("unsafe or missing event detail: %v", err)
	}
}

func assertDefinitionDNSAcceptedRecovery(t *testing.T, run definitionDNSFailureRun) {
	t.Helper()
	var claim string
	if err := run.pool.QueryRow(run.ctx, `SELECT token FROM eregion_target_claims WHERE operation_id=$1 AND sealed_at IS NOT NULL AND released_at IS NULL`, run.child).Scan(&claim); err != nil {
		t.Fatalf("uncertain write must retain a sealed claim: %v", err)
	}
	run.provider.mu.Lock()
	run.provider.readStatus = http.StatusForbidden
	run.provider.mu.Unlock()
	if err := runPlatformAttempt(run.ctx, run.service, run.child, 2); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("unresolved write must retry rejected read: %v", err)
	}
	var state contract.State
	var reserved bool
	if err := run.pool.QueryRow(run.ctx, `SELECT c.state,r.finished_at IS NULL FROM eregion_operations c JOIN eregion_definition_runs r ON r.operation_id=$2 WHERE c.id=$1`, run.child, run.parent).Scan(&state, &reserved); err != nil || state.Terminal() || !reserved {
		t.Fatalf("rejected read state=%s reserved=%v err=%v", state, reserved, err)
	}
	var retained bool
	if err := run.pool.QueryRow(run.ctx, `SELECT EXISTS (SELECT 1 FROM eregion_target_claims WHERE operation_id=$1 AND token=$2 AND sealed_at IS NOT NULL AND released_at IS NULL)`, run.child, claim).Scan(&retained); err != nil || !retained {
		t.Fatalf("rejected read claim retained=%v err=%v", retained, err)
	}
	assertDefinitionDNSFailureWrites(t, run)
	run.provider.mu.Lock()
	run.provider.readStatus = 0
	run.provider.mu.Unlock()
	if err := runPlatformAttempt(run.ctx, run.service, run.child, 3); err != nil {
		t.Fatalf("uncertain write readback: %v", err)
	}
	if err := run.pool.QueryRow(run.ctx, `SELECT state FROM eregion_operations WHERE id=$1`, run.child).Scan(&state); err != nil || state != contract.Succeeded {
		t.Fatalf("reconciled state=%s err=%v", state, err)
	}
	if err := run.pool.QueryRow(run.ctx, `SELECT EXISTS (SELECT 1 FROM eregion_target_claims WHERE operation_id=$1 AND released_at IS NULL)`, run.child).Scan(&retained); err != nil || retained {
		t.Fatalf("reconciled claim retained=%v err=%v", retained, err)
	}
}

func assertDefinitionDNSRateLimitCancellation(t *testing.T, run definitionDNSFailureRun) {
	t.Helper()
	if _, err := run.service.RequestCancel(run.ctx, definitionIntegrationActor, run.parent); err != nil {
		t.Fatal(err)
	}
	err := run.worker.runDefinitionApply(run.ctx, run.claimed, 1)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("parent did not wait for child: %v", err)
	}
	var parentState, childState contract.State
	var reserved bool
	if err := run.pool.QueryRow(run.ctx, `SELECT p.state,c.state,r.finished_at IS NULL FROM eregion_operations p JOIN eregion_definition_runs r ON r.operation_id=p.id JOIN eregion_operations c ON c.id=$2 WHERE p.id=$1`, run.parent, run.child).Scan(&parentState, &childState, &reserved); err != nil ||
		parentState != contract.Cancelling || childState != contract.Cancelling || !reserved {
		t.Fatalf("parent=%s child=%s reserved=%v err=%v", parentState, childState, reserved, err)
	}
}

func assertDefinitionDNSFailureWrites(t *testing.T, run definitionDNSFailureRun) {
	t.Helper()
	run.provider.mu.Lock()
	writes := run.provider.writes
	run.provider.mu.Unlock()
	if writes != 1 {
		t.Fatalf("writes=%d", writes)
	}
}
