package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type resultTestDNSConfig struct{}

func (resultTestDNSConfig) DNSZone(_ context.Context, id, name string) (integrations.DNSZone, bool, error) {
	return integrations.DNSZone{ZoneID: id, ZoneName: name}, true, nil
}
func (resultTestDNSConfig) DNSZones(context.Context) ([]integrations.DNSZone, error) {
	return nil, nil
}
func (resultTestDNSConfig) DNSCredential(context.Context, string, string) ([]byte, error) {
	return []byte("test-only-cloudflare-token-123456789"), nil
}

func TestIntegrationRegisteredExtensionIdentityAndCollisionResult(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	run.Service.redaction = &secrets.Registry{}
	run.Service.redaction.Add("a", "stdout", "rawsecret")
	audit, err := integrations.NewPostgresAudit(pool)
	if err != nil {
		t.Fatal(err)
	}
	store, err := integrations.NewPostgresGrantStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	access, err := integrations.NewAccessController(integrations.AccessConfig{
		AdminSubjects: []string{testActor.Subject}, Audit: audit, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := integrations.NewStaticAuthorizer(map[string][]string{testActor.Subject: {"*"}})
	if err != nil {
		t.Fatal(err)
	}
	dnsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, `{"success":true,"result":[],"result_info":{"page":1,"total_pages":1}}`)
		case http.MethodPost:
			fmt.Fprint(w, `{"success":true,"result":{"id":"fixture","type":"A","name":"app.alpha.example","content":"192.0.2.1","ttl":1,"comment":"eregion:managed"}}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer dnsServer.Close()
	registry := platform.NewRegistry()
	config := resultTestDNSConfig{}
	if err := integrations.Configure(registry, integrations.RuntimeConfig{Audit: audit, Access: access,
		Authorizer: authorizer, Zones: config, Credentials: config, CloudflareBaseURL: dnsServer.URL}); err != nil {
		t.Fatal(err)
	}
	execute := func(kind string, input any) (contract.Operation, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := registry.Normalize(kind, "platform", raw)
		if err != nil {
			t.Fatal(err)
		}
		id, err := contract.NewOperationID()
		if err != nil {
			t.Fatal(err)
		}
		// A real registered executor, PostgreSQL grant store, audit and service
		// settlement are used; this isolated registry avoids global test state.
		_, err = pool.Exec(ctx, `INSERT INTO eregion_operations
			(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,error_code,created_at,updated_at)
			VALUES($1,$2,'platform',$3,$4,$5,'fixture',$6,'running',1,0,0,'',clock_timestamp(),clock_timestamp())`,
			id, kind, testActor.Kind, testActor.Subject, string(id), normalized.Canonical)
		if err != nil {
			t.Fatal(err)
		}
		_, owned, err := run.Service.mutateOwned(ctx, id, 1, func(tx pgx.Tx, op *contract.Operation) error {
			return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "integration", Message: "Integration attempt started."})
		})
		if err != nil || !owned {
			t.Fatalf("fixture claim: %v %v", owned, err)
		}
		if err := (&platformWorker{service: run.Service}).runExtension(ctx, id, 1, normalized, normalized.Executor()); err != nil {
			t.Fatal(err)
		}
		op, err := run.Service.Get(ctx, testActor, id)
		if err != nil || op.State != contract.Succeeded || op.LatestResult == nil || op.LatestResult.DataStatus != "available" {
			t.Fatalf("operation: %+v %v", op, err)
		}
		var data map[string]any
		if err := json.Unmarshal(op.LatestResult.Data, &data); err != nil {
			t.Fatal(err)
		}
		return op, data
	}
	_, granted := execute(integrations.KindAccessGrant, map[string]any{
		"subject": "alice", "device_id": "alpha", "routes": []string{"app.example"},
		"expires_at": time.Now().UTC().Add(2 * time.Hour),
	})
	id, ok := granted["id"].(string)
	routes, routesOK := granted["routes"].([]any)
	if !ok || !strings.HasPrefix(id, "grant_") || strings.Contains(id, "[REDACTED]") ||
		granted["subject"] != "alice" || granted["device_id"] != "alpha" ||
		!routesOK || len(routes) != 1 || routes[0] != "app.example" || granted["expires_at"] == nil {
		t.Fatalf("grant result changed structure or identity: %+v", granted)
	}
	stored, found, err := store.Get(ctx, id)
	if err != nil || !found || stored.Subject != "alice" || stored.DeviceID != "alpha" {
		t.Fatalf("grant not durably created: %+v %v %v", stored, found, err)
	}
	_, revoked := execute(integrations.KindAccessRevoke, map[string]any{"grant_id": id})
	if revoked["id"] != id || revoked["revoked_at"] == nil {
		t.Fatalf("revoke identity/schema: %+v", revoked)
	}
	dnsOp, dns := execute(integrations.KindDNSReconcile, map[string]any{
		"zone_id": strings.Repeat("a", 32), "zone_name": "alpha.example",
		"records": []map[string]any{{"type": "A", "name": "app.alpha.example", "content": "192.0.2.1"}},
	})
	if dns["zone"] != "alpha.example" || dns["operation_id"] != string(dnsOp.ID) ||
		dns["changes"] != float64(1) || dns["applied"] != float64(1) || dns["unchanged"] != float64(0) {
		t.Fatalf("DNS result changed structure or identity: %+v", dns)
	}

	op, err := run.Service.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget,
		Actor: testActor, IdempotencyKey: "extension-key-collision", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	executor := platform.ExecutorFunc(func(context.Context, platform.Execution) (platform.Result, error) {
		return platform.Result{Data: map[string]any{
			"a": "first", "[REDACTED]": "second", "stdout": "rawsecret", "id": "grant_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}}, nil
	})
	if err := (&platformWorker{service: run.Service}).runExtension(ctx, op.ID, 1,
		platform.Normalized{Kind: op.Kind, Target: platform.PlatformTarget(), Summary: "extension"}, executor); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.LatestResult == nil || got.LatestResult.DataStatus != "partial" {
		t.Fatalf("collision outcome: %+v %v", got, err)
	}
	var data map[string]any
	if err := json.Unmarshal(got.LatestResult.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["redaction_omissions"] != float64(1) || data["stdout"] == nil ||
		strings.Contains(string(got.LatestResult.Data), "rawsecret") ||
		data["id"] == "grant_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("untrusted collision or text was hidden/bypassed: %s", got.LatestResult.Data)
	}
}
