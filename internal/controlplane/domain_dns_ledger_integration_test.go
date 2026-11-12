package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type domainDNSLedgerFixture struct {
	mu                      sync.Mutex
	records                 map[string]integrations.DNSRecord
	reappearDelete          bool
	failReadAfterDelete     bool
	failReadHost            string
	failDeleteHost          string
	failReadAfterDeleteHost string
	afterDelete             func()
	status                  int
}

func (f *domainDNSLedgerFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ledger-token-0123456789" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		http.Error(w, "injected provider failure", f.status)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if f.failReadHost != "" && r.URL.Query().Get("name") == f.failReadHost {
			http.Error(w, "injected hostname read failure", http.StatusInternalServerError)
			return
		}
		values := make([]integrations.DNSRecord, 0, len(f.records))
		for _, record := range f.records {
			values = append(values, record)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "errors": []any{}, "result": values,
			"result_info": map[string]any{"page": 1, "total_pages": 1},
		})
	case http.MethodDelete:
		id := filepath.Base(r.URL.Path)
		record, existed := f.records[id]
		if existed && f.failDeleteHost != "" && record.Name == f.failDeleteHost {
			http.Error(w, "injected delete failure", http.StatusInternalServerError)
			return
		}
		delete(f.records, id)
		if existed && f.reappearDelete {
			record.ID = id + "-reappeared"
			f.records[record.ID] = record
		}
		if f.failReadAfterDelete || (existed && f.failReadAfterDeleteHost != "" && record.Name == f.failReadAfterDeleteHost) {
			f.status = http.StatusInternalServerError
		}
		if f.afterDelete != nil {
			f.afterDelete()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": map[string]string{"id": id}})
	default:
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	}
}

func TestIntegrationManagedDNSLedgerPersistsAndCleansUp(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()

	fixture := &domainDNSLedgerFixture{records: map[string]integrations.DNSRecord{}}
	server := httptest.NewServer(fixture)
	defer server.Close()
	store := newTestDNSStore(integrations.DNSZone{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com"}).withToken("0123456789abcdef0123456789abcdef", "ledger-token-0123456789")
	domainActor := contract.Actor{Kind: "operator", Subject: testActor.Subject}
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: store, Credentials: store,
		BaseURL:    server.URL + "/client/v4",
		HTTPClient: server.Client(),
		Authorizer: integrations.NewOperatorDNSAuthorizer(),
		Audit:      integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.domainDNS = domainDNS

	platformSubmit(t, s, platform.KindAppCreate, "platform", "ledger-app", map[string]any{"name": "ledger"})
	envCreateOp := platformSubmit(t, s, platform.KindEnvironmentCreate, "app/ledger", "ledger-env", map[string]any{
		"name": "test", "node": "local",
	})
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, envCreateOp.ID); err != nil {
		t.Fatal(err)
	}
	readOnly, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: store, Credentials: store,
		BaseURL: server.URL + "/client/v4", HTTPClient: server.Client(),
		Authorizer: integrations.NewOperatorDNSAuthorizer(), Audit: integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.domainDNS = readOnly
	proxied := true
	if _, err := s.Submit(ctx, platformSubmissionAs(domainActor, platform.KindDomainAdd, "env/ledger/test", "ledger-outside-zone", map[string]any{
		"domain": "app.other.example", "dns_mode": "cloudflare",
		"dns_zone": "example.com", "dns_origins": []string{"8.8.8.8"}, "dns_proxied": proxied,
	})); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("outside-zone managed DNS admission = %v", err)
	}
	if _, err := s.Submit(ctx, platformSubmissionAs(contract.Actor{Kind: "ci", Subject: domainActor.Subject}, platform.KindDomainAdd, "env/ledger/test", "ledger-denied", map[string]any{
		"domain": "app.example.com", "dns_mode": "cloudflare",
		"dns_zone": "example.com", "dns_origins": []string{"8.8.8.8"}, "dns_proxied": proxied,
	})); contract.Classify(err).Code != contract.Forbidden {
		t.Fatalf("unauthorized managed DNS admission = %v", err)
	}
	var deniedOps, deniedLedgers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1 AND target='env/ledger/test'`, platform.KindDomainAdd).Scan(&deniedOps); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_domain_dns`).Scan(&deniedLedgers); err != nil {
		t.Fatal(err)
	}
	if deniedOps != 0 || deniedLedgers != 0 {
		t.Fatalf("unauthorized admission left durable state: operations=%d ledgers=%d", deniedOps, deniedLedgers)
	}
	s.domainDNS = domainDNS
	addOp := platformSubmitAs(t, s, domainActor, platform.KindDomainAdd, "env/ledger/test", "ledger-add", map[string]any{
		"domain": "app.example.com", "dns_mode": "cloudflare",
		"dns_zone": "example.com", "dns_origins": []string{"8.8.8.8"}, "dns_proxied": proxied,
	})
	var domainID string
	var generation int64
	var origins []string
	var storedProxied bool
	var state, lastOperation, marker, zoneID string
	if err := pool.QueryRow(ctx, `SELECT d.id,x.generation,x.desired_origins,x.desired_proxied,x.state,x.last_operation_id,x.ownership_marker,x.zone_id
        FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id
        WHERE d.name='app.example.com'`).Scan(&domainID, &generation, &origins, &storedProxied, &state, &lastOperation, &marker, &zoneID); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || len(origins) != 1 || origins[0] != "8.8.8.8" || !storedProxied || state != domainDNSStatePending || lastOperation != string(addOp.ID) {
		t.Fatalf("ledger intent = id=%s generation=%d origins=%v proxied=%v state=%s op=%s", domainID, generation, origins, storedProxied, state, lastOperation)
	}
	if marker != integrations.DomainDNSOwnership(domainID, zoneID) {
		t.Fatalf("marker = %q, want immutable identity marker", marker)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, addOp.ID); err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/ledger", "ledger-other", map[string]any{
		"name": "other", "node": "local",
	})
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE idempotency_key='ledger-other'`); err != nil {
		t.Fatal(err)
	}
	var envID string
	if err := pool.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='ledger' AND e.name='test'`).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	deleteOp := platformSubmitAs(t, s, domainActor, platform.KindEnvironmentDelete, "env/ledger/test", "ledger-delete", map[string]any{
		"confirm": "ledger/test",
	})
	ledger, err := s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil {
		t.Fatalf("cleanup ledger = %+v, %v", ledger, err)
	}
	if ledger.State != domainDNSStateCleanupPending || ledger.LastOperationID != deleteOp.ID {
		t.Fatalf("cleanup intent = %+v", ledger)
	}
	fixture.mu.Lock()
	fixture.records["owned"] = integrations.DNSRecord{
		ID: "owned", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1,
		Proxied: true, Comment: ledger.Ownership,
	}
	fixture.mu.Unlock()
	op, _, err := s.operationSnapshot(ctx, deleteOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.cleanupDomainDNSForEnvironments(ctx, op, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("cancellation boundary error = %v", err)
	}
	fixture.mu.Lock()
	_, providerRetained := fixture.records["owned"]
	fixture.mu.Unlock()
	if !providerRetained {
		t.Fatal("provider record was mutated even though cancellation was accepted before cleanup")
	}
	ledger, err = s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupPending || ledger.LastOperationID != deleteOp.ID {
		t.Fatalf("cancellation did not retain cleanup state: %+v, %v", ledger, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running' WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET attempt=attempt+1 WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	newerOp, _, err := s.operationSnapshot(ctx, deleteOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newerOp.Attempt != op.Attempt+1 {
		t.Fatalf("takeover snapshot attempt = %d, want %d", newerOp.Attempt, op.Attempt+1)
	}
	// The worker still owns its originally claimed attempt. Reloading the
	// operation after a takeover must not let newerOp.Attempt pass as ours.
	if err := s.cleanupDomainDNSForEnvironments(ctx, newerOp, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); !errors.Is(err, errNotOwned) {
		t.Fatalf("takeover cleanup attempt fence error = %v", err)
	}
	fixture.mu.Lock()
	_, providerRetained = fixture.records["owned"]
	fixture.mu.Unlock()
	if !providerRetained {
		t.Fatal("provider record was mutated by a taken-over worker")
	}
	if err := s.applyDomainDNSEffect(ctx, newerOp, op.Attempt, &platform.DomainInput{
		App: "ledger", Environment: "test", Domain: "app.example.com",
	}, true); !errors.Is(err, errNotOwned) {
		t.Fatalf("takeover domain effect attempt fence error = %v", err)
	}
	fixture.mu.Lock()
	_, providerRetained = fixture.records["owned"]
	fixture.mu.Unlock()
	if !providerRetained {
		t.Fatal("provider record was mutated by a taken-over domain effect")
	}
	ledger, err = s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupPending {
		t.Fatalf("takeover mutated cleanup ledger: %+v, %v", ledger, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET attempt=attempt-1 WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.reappearDelete = true
	fixture.mu.Unlock()
	if err := s.cleanupDomainDNSForEnvironments(ctx, op, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("reappeared provider record cleanup = %v", err)
	}
	ledger, err = s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupFailed {
		t.Fatalf("reappearance did not retain failed cleanup ledger: %+v, %v", ledger, err)
	}
	fixture.mu.Lock()
	fixture.reappearDelete = false
	fixture.failReadAfterDelete = true
	fixture.mu.Unlock()
	if err := s.cleanupDomainDNSForEnvironments(ctx, op, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("post-delete read failure cleanup = %v", err)
	}
	ledger, err = s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupFailed {
		t.Fatalf("post-delete read failure did not retain cleanup ledger: %+v, %v", ledger, err)
	}
	fixture.mu.Lock()
	fixture.failReadAfterDelete = false
	fixture.status = 0
	fixture.mu.Unlock()
	if err := s.cleanupDomainDNSForEnvironments(ctx, op, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteDomainRowsOwned(ctx, op.ID, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("binding deletion cancellation boundary error = %v", err)
	}
	ledger, err = s.loadDomainDNSLedger(ctx, "ledger", "test", "app.example.com")
	if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupVerified {
		t.Fatalf("cancellation did not retain verified cleanup state: %+v, %v", ledger, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running' WHERE id=$1`, deleteOp.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteDomainRowsOwned(ctx, op.ID, op.Attempt, []platform.EnvironmentID{platform.EnvironmentID(envID)}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	remaining := len(fixture.records)
	fixture.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("provider record remained after verified cleanup: %+v", fixture.records)
	}
	var ledgers, domains int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_domain_dns`).Scan(&ledgers); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_domains WHERE name='app.example.com'`).Scan(&domains); err != nil {
		t.Fatal(err)
	}
	if ledgers != 0 || domains != 0 {
		t.Fatalf("cleanup rows remain: ledgers=%d domains=%d", ledgers, domains)
	}
}
