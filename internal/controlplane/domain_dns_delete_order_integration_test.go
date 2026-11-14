package controlplane

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationDeleteOrderKeepsServingWorkloadsWhenCloudflareCleanupFails(t *testing.T) {
	for _, test := range []struct {
		name       string
		app        string
		deleteKind string
		call       func(*testing.T, *platformWorker, contract.Operation, string)
	}{
		{
			name: "environment", app: "order-env", deleteKind: platform.KindEnvironmentDelete,
			call: func(t *testing.T, w *platformWorker, op contract.Operation, app string) {
				if err := w.deleteEnvironment(context.Background(), op.ID, op.Attempt, app, "test"); err == nil {
					t.Fatal("provider failure did not fail environment deletion")
				}
			},
		},
		{
			name: "application", app: "order-app", deleteKind: platform.KindAppDelete,
			call: func(t *testing.T, w *platformWorker, op contract.Operation, app string) {
				if err := w.deleteApplication(context.Background(), op.ID, op.Attempt, app); err == nil {
					t.Fatal("provider failure did not fail application deletion")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool, _ := testPool(t, true)
			transport := &scriptedPlatformTransport{}
			identity := ensureTestRegistryHost(t, pool, t.TempDir())
			run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
				Concurrency: 0, TestOnly: true, Transport: transport,
				NodeStateDir: identity.StateDir, RegistryHostIdentity: &identity,
				RegistryEndpoint: "127.0.0.1:5000",
			})
			if err != nil {
				t.Fatal(err)
			}
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
			proxied := true

			platformSubmit(t, s, platform.KindNodeRegister, "platform", "delete-order-node", map[string]any{
				"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
			})
			platformSubmit(t, s, platform.KindAppCreate, "platform", "delete-order-app", map[string]any{"name": test.app})
			for _, envName := range []string{"test", "other"} {
				op := platformSubmit(t, s, platform.KindEnvironmentCreate, "app/"+test.app, "delete-order-env-"+envName, map[string]any{
					"name": envName, "node": "local",
				})
				if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, op.ID); err != nil {
					t.Fatal(err)
				}
			}
			addOp := platformSubmitAs(t, s, domainActor, platform.KindDomainAdd, "env/"+test.app+"/test", "delete-order-domain", map[string]any{
				"domain": "app.example.com", "dns_mode": "cloudflare", "dns_zone": "example.com",
				"dns_origins": []string{"8.8.8.8"}, "dns_proxied": proxied,
			})
			if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, addOp.ID); err != nil {
				t.Fatal(err)
			}
			ledger, err := s.loadDomainDNSLedger(ctx, test.app, "test", "app.example.com")
			if err != nil || ledger == nil {
				t.Fatalf("ledger = %+v, %v", ledger, err)
			}
			fixture.mu.Lock()
			fixture.records["owned"] = integrations.DNSRecord{
				ID: "owned", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1,
				Proxied: true, Comment: ledger.Ownership,
			}
			fixture.status = 500
			fixture.mu.Unlock()

			var deleteOp contract.Operation
			if test.deleteKind == platform.KindAppDelete {
				s.humanFactor = func(string) error { return nil }
				submission := platformSubmissionAs(domainActor, platform.KindAppDelete, "app/"+test.app, "delete-order-app-op", map[string]any{"confirm": test.app})
				submission.HumanFactor = "test-factor"
				deleteOp, err = s.Submit(ctx, submission)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				deleteOp = platformSubmitAs(t, s, domainActor, platform.KindEnvironmentDelete, "env/"+test.app+"/test", "delete-order-env-op", map[string]any{"confirm": test.app + "/test"})
			}
			test.call(t, &platformWorker{service: s}, deleteOp, test.app)

			if got := transport.runCount("platform-delete-01-v1"); got != 0 {
				t.Fatalf("serving workload node step ran %d time(s) after provider cleanup failed", got)
			}
			if test.deleteKind == platform.KindEnvironmentDelete {
				if _, err := s.environment(ctx, test.app, "test"); err != nil {
					t.Fatalf("environment was removed after provider failure: %v", err)
				}
			} else if _, environments, err := s.applicationEnvironments(ctx, test.app); err != nil || len(environments) != 2 {
				t.Fatalf("application state was removed after provider failure: envs=%d err=%v", len(environments), err)
			}
			fixture.mu.Lock()
			_, providerRetained := fixture.records["owned"]
			fixture.mu.Unlock()
			if !providerRetained {
				t.Fatal("provider record was mutated while cleanup was failing")
			}
			// The provider read fails during all-binding preflight, so no
			// record was mutated and the ledger keeps its cleanup intent.
			ledger, err = s.loadDomainDNSLedger(ctx, test.app, "test", "app.example.com")
			if err != nil || ledger == nil || ledger.State != domainDNSStateCleanupPending || ledger.CleanupError != "" {
				t.Fatalf("preflight-refused cleanup ledger = %+v, %v", ledger, err)
			}
			text := operationEventText(t, s, deleteOp.ID)
			if !strings.Contains(text, "preflight refused") || !strings.Contains(text, "no workload was removed") {
				t.Fatalf("preflight failure event is not honest: %s", text)
			}
		})
	}
}

func setupMultiBindingDelete(t *testing.T, app string) (*Service, *scriptedPlatformTransport, *domainDNSLedgerFixture, context.Context, contract.Operation) {
	t.Helper()
	pool, _ := testPool(t, true)
	transport := &scriptedPlatformTransport{}
	identity := ensureTestRegistryHost(t, pool, t.TempDir())
	run, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, Transport: transport,
		NodeStateDir: identity.StateDir, RegistryHostIdentity: &identity,
		RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	// The test host has no Caddy; the workload/DNS ordering and retry are the
	// subject here, not the route writer.
	s.routeSync = func(context.Context) error { return nil }
	ctx := context.Background()
	fixture := &domainDNSLedgerFixture{records: map[string]integrations.DNSRecord{}}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	store := newTestDNSStore(integrations.DNSZone{ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com"}).withToken("0123456789abcdef0123456789abcdef", "ledger-token-0123456789")
	domainActor := contract.Actor{Kind: "operator", Subject: testActor.Subject}
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: store, Credentials: store,
		BaseURL: server.URL + "/client/v4", HTTPClient: server.Client(),
		Authorizer: integrations.NewOperatorDNSAuthorizer(), Audit: integrations.NewMemoryAudit(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.domainDNS = domainDNS
	proxied := true
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "multi-delete-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "multi-delete-app", map[string]any{"name": app})
	for _, envName := range []string{"test", "other"} {
		op := platformSubmit(t, s, platform.KindEnvironmentCreate, "app/"+app, "multi-delete-env-"+envName, map[string]any{"name": envName, "node": "local"})
		if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
	}
	for index, host := range []string{"a.example.com", "b.example.com"} {
		addOp := platformSubmitAs(t, s, domainActor, platform.KindDomainAdd, "env/"+app+"/test", "multi-delete-domain-"+string(rune('a'+index)), map[string]any{
			"domain": host, "dns_mode": "cloudflare", "dns_zone": "example.com",
			"dns_origins": []string{"8.8.8.8"}, "dns_proxied": proxied,
		})
		if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, addOp.ID); err != nil {
			t.Fatal(err)
		}
		ledger, err := s.loadDomainDNSLedger(ctx, app, "test", host)
		if err != nil || ledger == nil {
			t.Fatalf("ledger %s = %+v, %v", host, ledger, err)
		}
		fixture.mu.Lock()
		fixture.records["owned-"+host] = integrations.DNSRecord{
			ID: "owned-" + host, Name: host, Type: "A", Content: "8.8.8.8", TTL: 1, Proxied: true, Comment: ledger.Ownership,
		}
		fixture.mu.Unlock()
	}
	deleteOp := platformSubmitAs(t, s, domainActor, platform.KindEnvironmentDelete, "env/"+app+"/test", "multi-delete-op", map[string]any{"confirm": app + "/test"})
	// The direct worker-body call still needs the real attempt claim, including
	// its fence and event. A running operation with attempt zero is invalid.
	claimedOp, _, claimed, err := s.claimPlatform(ctx, deleteOp.ID, 1)
	if err != nil || !claimed || claimedOp.State != contract.Running || claimedOp.Attempt != 1 {
		t.Fatalf("claim deletion attempt: claimed=%v, %v", claimed, err)
	}
	deleteOp = claimedOp
	return s, transport, fixture, ctx, deleteOp
}

func assertDeletionResult(t *testing.T, s *Service, op contract.Operation, attempt uint32, app string, priorFailure bool) {
	t.Helper()
	current, err := s.Get(context.Background(), op.Actor, op.ID)
	if err != nil || current.State != contract.Succeeded || current.LatestResult == nil ||
		!current.LatestResult.Final || current.LatestResult.Status != contract.Succeeded ||
		current.LatestResult.Attempt != attempt || current.LatestResult.OperationID != op.ID ||
		current.LatestResult.Target != op.Target {
		t.Fatalf("deletion did not record the claimed successful result: %+v, %v", current, err)
	}
	if _, err := s.environment(context.Background(), app, "test"); err == nil || contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("deleted environment lookup must return not_found: %v", err)
	}
	if priorFailure {
		var retained bool
		if err := s.pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM eregion_operation_events
			WHERE operation_id=$1 AND event->'result'->>'attempt'='1'
			AND event->'result'->>'status'='failed' AND event->'result'->>'final'='false')`, op.ID).Scan(&retained); err != nil || !retained {
			t.Fatalf("earlier failed attempt was not retained: %v", err)
		}
	}
}

func providerRecordPresent(f *domainDNSLedgerFixture, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.records {
		if record.Name == name {
			return true
		}
	}
	return false
}

func operationEventText(t *testing.T, s *Service, id contract.OperationID) string {
	t.Helper()
	authority := operations.OperatorAuthority(contract.Actor{Kind: "operator", Subject: testActor.Subject})
	page, err := s.Events(context.Background(), authority, contract.Cursor{OperationID: id}, 200)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, event := range page.Events {
		text += event.Message + "\n"
	}
	return text
}

func TestIntegrationMultiBindingDNSCleanupPreflightsAll(t *testing.T) {
	s, transport, fixture, ctx, deleteOp := setupMultiBindingDelete(t, "multi-preflight")
	fixture.mu.Lock()
	fixture.records["foreign-b"] = integrations.DNSRecord{ID: "foreign-b", Name: "b.example.com", Type: "CNAME", Content: "other.example.net", TTL: 1, Comment: "manual"}
	// External records are preserved, not a cleanup refusal. A failed read of
	// the later binding must still stop cleanup before any owned record changes.
	fixture.failReadHost = "b.example.com"
	before := maps.Clone(fixture.records)
	fixture.mu.Unlock()
	w := &platformWorker{service: s}
	_ = w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-preflight", "test")
	if transport.runCount("platform-delete-01-v1") != 0 {
		t.Fatal("workload was removed after preflight read failure")
	}
	if !providerRecordPresent(fixture, "a.example.com") || !providerRecordPresent(fixture, "b.example.com") {
		t.Fatal("preflight read failure mutated provider records")
	}
	fixture.mu.Lock()
	unchanged := maps.Equal(before, fixture.records)
	fixture.mu.Unlock()
	if !unchanged {
		t.Fatal("preflight read failure changed owned or external provider records")
	}
	if _, err := s.environment(ctx, "multi-preflight", "test"); err != nil {
		t.Fatalf("environment was removed after preflight read failure: %v", err)
	}
	text := operationEventText(t, s, deleteOp.ID)
	if !strings.Contains(text, "preflight refused") || !strings.Contains(text, "no workload was removed") {
		t.Fatalf("preflight event is not honest: %s", text)
	}
}

func TestIntegrationMultiBindingDNSLaterFailureRetainsLedgerAndWorkloads(t *testing.T) {
	s, transport, fixture, ctx, deleteOp := setupMultiBindingDelete(t, "multi-later")
	fixture.mu.Lock()
	fixture.failDeleteHost = "b.example.com"
	fixture.mu.Unlock()
	w := &platformWorker{service: s}
	_ = w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-later", "test")
	if transport.runCount("platform-delete-01-v1") != 0 {
		t.Fatal("workload was removed after a later binding failed")
	}
	if providerRecordPresent(fixture, "a.example.com") || !providerRecordPresent(fixture, "b.example.com") {
		t.Fatal("partial provider state = want a removed and b retained")
	}
	if _, err := s.environment(ctx, "multi-later", "test"); err != nil {
		t.Fatalf("environment was removed after later binding failure: %v", err)
	}
	a, err := s.loadDomainDNSLedger(ctx, "multi-later", "test", "a.example.com")
	if err != nil || a == nil || a.State != domainDNSStateCleanupVerified {
		t.Fatalf("a ledger = %+v, %v", a, err)
	}
	b, err := s.loadDomainDNSLedger(ctx, "multi-later", "test", "b.example.com")
	if err != nil || b == nil || b.State != domainDNSStateCleanupFailed {
		t.Fatalf("b ledger = %+v, %v", b, err)
	}
	text := operationEventText(t, s, deleteOp.ID)
	if !strings.Contains(text, "stopped after 1 of 2") || !strings.Contains(text, "not atomic") || !strings.Contains(text, "retry") {
		t.Fatalf("partial cleanup event is not honest: %s", text)
	}
	fixture.mu.Lock()
	fixture.failDeleteHost = ""
	fixture.mu.Unlock()
	retry, _, claimed, err := s.claimPlatform(ctx, deleteOp.ID, 2)
	if err != nil || !claimed || retry.State != contract.Running || retry.Attempt != 2 {
		t.Fatalf("claim second deletion attempt: claimed=%v, %v", claimed, err)
	}
	deleteOp = retry
	if err := w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-later", "test"); err != nil {
		t.Fatalf("retry after later failure = %v", err)
	}
	if transport.runCount("platform-delete-01-v1") != 1 {
		t.Fatal("retry did not resume workload deletion")
	}
	if _, err := s.environment(ctx, "multi-later", "test"); err == nil {
		t.Fatal("retry did not delete the environment")
	}
	if providerRecordPresent(fixture, "a.example.com") || providerRecordPresent(fixture, "b.example.com") {
		t.Fatal("retry left provider records")
	}
	assertDeletionResult(t, s, deleteOp, 2, "multi-later", true)
}

func TestIntegrationMultiBindingDNSPostDeleteReadFailureRetainsLedger(t *testing.T) {
	s, transport, fixture, ctx, deleteOp := setupMultiBindingDelete(t, "multi-read")
	fixture.mu.Lock()
	fixture.failReadAfterDeleteHost = "b.example.com"
	fixture.mu.Unlock()
	w := &platformWorker{service: s}
	_ = w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-read", "test")
	if transport.runCount("platform-delete-01-v1") != 0 {
		t.Fatal("workload was removed after post-delete read failure")
	}
	if providerRecordPresent(fixture, "a.example.com") || providerRecordPresent(fixture, "b.example.com") {
		t.Fatal("provider records remain after delete requests")
	}
	b, err := s.loadDomainDNSLedger(ctx, "multi-read", "test", "b.example.com")
	if err != nil || b == nil || b.State != domainDNSStateCleanupFailed {
		t.Fatalf("b ledger = %+v, %v", b, err)
	}
	if _, err := s.environment(ctx, "multi-read", "test"); err != nil {
		t.Fatalf("environment was removed after read failure: %v", err)
	}
	text := operationEventText(t, s, deleteOp.ID)
	if !strings.Contains(text, "not atomic") || !strings.Contains(text, "retry") {
		t.Fatalf("partial cleanup event is not honest: %s", text)
	}
	fixture.mu.Lock()
	fixture.failReadAfterDeleteHost = ""
	fixture.status = 0
	fixture.mu.Unlock()
	retry, _, claimed, err := s.claimPlatform(ctx, deleteOp.ID, 2)
	if err != nil || !claimed || retry.State != contract.Running || retry.Attempt != 2 {
		t.Fatalf("claim second deletion attempt: claimed=%v, %v", claimed, err)
	}
	deleteOp = retry
	if err := w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-read", "test"); err != nil {
		t.Fatalf("retry after read failure = %v", err)
	}
	if transport.runCount("platform-delete-01-v1") != 1 {
		t.Fatal("retry did not resume workload deletion")
	}
	if _, err := s.environment(ctx, "multi-read", "test"); err == nil {
		t.Fatal("retry did not delete the environment")
	}
	assertDeletionResult(t, s, deleteOp, 2, "multi-read", true)
}

func TestIntegrationMultiBindingDNSCancelRetainsLedgerAndRetries(t *testing.T) {
	s, transport, fixture, ctx, deleteOp := setupMultiBindingDelete(t, "multi-cancel")
	fixture.mu.Lock()
	fixture.afterDelete = func() {
		_, _ = s.pool.Exec(context.Background(), `UPDATE eregion_operations SET state='cancelling' WHERE id=$1`, deleteOp.ID)
	}
	fixture.mu.Unlock()
	w := &platformWorker{service: s}
	_ = w.deleteEnvironment(ctx, deleteOp.ID, deleteOp.Attempt, "multi-cancel", "test")
	if transport.runCount("platform-delete-01-v1") != 0 {
		t.Fatal("workload was removed after mid-cleanup cancellation")
	}
	if providerRecordPresent(fixture, "a.example.com") || !providerRecordPresent(fixture, "b.example.com") {
		t.Fatal("partial provider state = want a removed and b retained")
	}
	a, err := s.loadDomainDNSLedger(ctx, "multi-cancel", "test", "a.example.com")
	if err != nil || a == nil || a.State != domainDNSStateCleanupPending {
		t.Fatalf("a ledger after cancellation = %+v, %v", a, err)
	}
	b, err := s.loadDomainDNSLedger(ctx, "multi-cancel", "test", "b.example.com")
	if err != nil || b == nil || b.State != domainDNSStateCleanupPending {
		t.Fatalf("b ledger after cancellation = %+v, %v", b, err)
	}
	text := operationEventText(t, s, deleteOp.ID)
	if !strings.Contains(text, "not atomic") || !strings.Contains(text, "retry") {
		t.Fatalf("partial cleanup event is not honest: %s", text)
	}
	fixture.mu.Lock()
	fixture.afterDelete = nil
	fixture.mu.Unlock()
	retryOp := platformSubmitAs(t, s, contract.Actor{Kind: "operator", Subject: testActor.Subject}, platform.KindEnvironmentDelete, "env/multi-cancel/test", "multi-cancel-retry", map[string]any{"confirm": "multi-cancel/test"})
	claimedRetry, _, claimed, err := s.claimPlatform(ctx, retryOp.ID, 1)
	if err != nil || !claimed || claimedRetry.State != contract.Running || claimedRetry.Attempt != 1 {
		t.Fatalf("claim deletion retry: claimed=%v, %v", claimed, err)
	}
	retryOp = claimedRetry
	if err := w.deleteEnvironment(ctx, retryOp.ID, retryOp.Attempt, "multi-cancel", "test"); err != nil {
		t.Fatalf("retry after cancellation = %v", err)
	}
	if transport.runCount("platform-delete-01-v1") != 1 {
		t.Fatal("retry after cancellation did not resume workload deletion")
	}
	if _, err := s.environment(ctx, "multi-cancel", "test"); err == nil {
		t.Fatal("retry after cancellation did not delete the environment")
	}
	assertDeletionResult(t, s, retryOp, 1, "multi-cancel", false)
	original, err := s.Get(ctx, deleteOp.Actor, deleteOp.ID)
	if err != nil || original.State != contract.Canceled {
		t.Fatalf("replacement operation rewrote the original cancellation: %+v, %v", original, err)
	}
}
