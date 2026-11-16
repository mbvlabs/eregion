package controlplane

import (
	"context"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

// testPool creates and drops an isolated database; run only against an
// explicitly authorized EREGION_TEST_DATABASE_URL.
func TestIntegrationDefinitionDNSPolicyAbsentCASAndReadError(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	store := NewDefinitionDNSPolicyStore(newTestEntities(t, pool))
	actor := definitionIntegrationActor
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_definition_dns_policy`); err != nil {
		t.Fatal(err)
	}
	initial, err := store.GetDefinitionDNSPolicy(ctx, actor)
	if err != nil || initial.Revision != 0 || initial.Enabled || initial.ProxyMode != "dns-only" {
		t.Fatalf("initial policy: %#v, %v", initial, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{})
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if contract.Classify(err).Code == contract.Conflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS successes=%d conflicts=%d", successes, conflicts)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE eregion_definition_dns_policy RENAME TO unavailable_definition_dns_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDefinitionDNSPolicy(ctx, actor); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing table error: %v", err)
	}
}

// The policy update and its audit entry share one entities transaction. A stale
// revision, an active definition run, and a failing audit insert each leave the
// policy revision and audit history unchanged.
func TestIntegrationDefinitionDNSPolicyAuditAtomicityAndFences(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	store := NewDefinitionDNSPolicyStore(newTestEntities(t, pool))
	actor := definitionIntegrationActor
	audits := func() int64 {
		t.Helper()
		var count int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit
			WHERE entry->>'action'='cloudflare.definition-dns-policy' AND entry->>'outcome'='succeeded'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	revision := func() int64 {
		t.Helper()
		var current int64
		if err := pool.QueryRow(ctx, `SELECT revision FROM eregion_definition_dns_policy WHERE singleton=true`).Scan(&current); err != nil {
			t.Fatal(err)
		}
		return current
	}

	policy, err := store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{ExpectedRevision: 0})
	if err != nil || policy.Revision != 1 || policy.Enabled {
		t.Fatalf("initial policy = %+v, %v", policy, err)
	}
	if got := audits(); got != 1 || revision() != 1 {
		t.Fatalf("initial policy audit=%d revision=%d", got, revision())
	}

	if _, err := store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{ExpectedRevision: 0}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale revision = %v", err)
	}
	if got := audits(); got != 1 || revision() != 1 {
		t.Fatalf("stale revision audit=%d revision=%d", got, revision())
	}

	if _, err := pool.Exec(ctx, `CREATE FUNCTION eregion_test_fail_policy_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'policy audit unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER eregion_test_fail_policy_audit BEFORE INSERT ON eregion_integration_audit
		FOR EACH ROW EXECUTE FUNCTION eregion_test_fail_policy_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{ExpectedRevision: 1}); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("audit failure = %v", err)
	}
	if got := audits(); got != 1 || revision() != 1 {
		t.Fatalf("audit failure audit=%d revision=%d", got, revision())
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER eregion_test_fail_policy_audit ON eregion_integration_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION eregion_test_fail_policy_audit()`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
		(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,error_code,created_at,updated_at)
		VALUES('op_active_policy_fixture','fixture','platform','system','fixture','active-policy-fixture','fixture','{}','queued',0,0,0,'',clock_timestamp(),clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_definition_runs
		(operation_id,plan_nonce,environment_id,approved,next_step,failure_code,failure_message,finished_at)
		VALUES('op_active_policy_fixture','nonce-active-policy','env-active-policy','{}'::jsonb,0,'','',NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{ExpectedRevision: 1}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("active definition run = %v", err)
	}
	if got := audits(); got != 1 || revision() != 1 {
		t.Fatalf("active run audit=%d revision=%d", got, revision())
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_definition_runs SET finished_at=clock_timestamp() WHERE operation_id='op_active_policy_fixture'`); err != nil {
		t.Fatal(err)
	}

	policy, err = store.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{ExpectedRevision: 1})
	if err != nil || policy.Revision != 2 || policy.Enabled {
		t.Fatalf("released policy = %+v, %v", policy, err)
	}
	if got := audits(); got != 2 || revision() != 2 {
		t.Fatalf("released policy audit=%d revision=%d", got, revision())
	}
}

// The enabled policy path uses a real vault-sealed provider credential and the
// real entities/vault/service transaction. It exercises the exact provider-zone
// share lock, successful enable, missing and ambiguous zone fences, and an
// enable whose audit insert is rejected.
func TestIntegrationDefinitionDNSPolicyEnabledPath(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	entitiesStore := newTestEntities(t, pool)
	policyStore := NewDefinitionDNSPolicyStore(entitiesStore)
	actor := definitionIntegrationActor
	providerActor := contract.Actor{Kind: "operator", Subject: "policy-enabled-provider"}
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewService(entitiesStore.Secrets(), entitiesStore.Secrets(), ring, entitiesStore.Providers())
	if err != nil {
		t.Fatal(err)
	}
	const zoneID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const ambiguousID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := setTestProviderCredential(t, ctx, entitiesStore, secrets, providerActor, zoneID, "enabled.example.com", 0); err != nil {
		t.Fatal(err)
	}
	audits := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_integration_audit
			WHERE entry->>'action'='cloudflare.definition-dns-policy' AND entry->>'outcome'='succeeded'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	current := func() api.DefinitionDNSPolicy {
		t.Helper()
		policy, err := policyStore.GetDefinitionDNSPolicy(ctx, actor)
		if err != nil {
			t.Fatalf("read policy: %v", err)
		}
		return policy
	}
	assertInitial := func() {
		t.Helper()
		policy := current()
		if policy.Revision != 1 || !policy.Enabled || policy.ZoneID != zoneID || policy.ZoneName != "enabled.example.com" ||
			len(policy.Origins) != 1 || policy.Origins[0] != "8.8.8.8" || policy.ProxyMode != "dns-only" {
			t.Fatalf("policy after fence = %+v", policy)
		}
		if got := audits(); got != 1 {
			t.Fatalf("policy audit count after fence = %d", got)
		}
	}

	policy, err := policyStore.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{
		ExpectedRevision: 0, Enabled: true, Zone: zoneID,
		Origins: []string{"8.8.8.8"}, ProxyMode: "dns-only",
	})
	if err != nil || policy.Revision != 1 || !policy.Enabled || policy.ZoneID != zoneID ||
		policy.ZoneName != "enabled.example.com" || len(policy.Origins) != 1 ||
		policy.Origins[0] != "8.8.8.8" || policy.ProxyMode != "dns-only" {
		t.Fatalf("enabled policy = %+v, %v", policy, err)
	}
	if got := audits(); got != 1 {
		t.Fatalf("enabled policy audit = %d", got)
	}
	var provider, storedZone, proxyMode string
	var enabled bool
	var origins []string
	if err := pool.QueryRow(ctx, `SELECT provider,zone_id,enabled,origins,proxy_mode
		FROM eregion_definition_dns_policy WHERE singleton=true`).
		Scan(&provider, &storedZone, &enabled, &origins, &proxyMode); err != nil ||
		provider != "cloudflare" || storedZone != zoneID || !enabled ||
		len(origins) != 1 || origins[0] != "8.8.8.8" || proxyMode != "dns-only" {
		t.Fatalf("enabled policy row = %q %q %v %v %q, %v", provider, storedZone, enabled, origins, proxyMode, err)
	}

	if _, err := policyStore.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{
		ExpectedRevision: 1, Enabled: true, Zone: "missing.example.com",
		Origins: []string{"8.8.4.4"}, ProxyMode: "dns-only",
	}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("missing zone = %v", err)
	}
	assertInitial()

	// An explicit corrupt provider row whose zone name equals the valid zone ID
	// makes the reference ambiguous without weakening any constraint.
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
		(provider,zone_id,zone_name,latest_revision,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
		VALUES('cloudflare',$1,$2,1,'operator','test','operator','test',clock_timestamp(),clock_timestamp())`, ambiguousID, zoneID); err != nil {
		t.Fatal(err)
	}
	if _, err := policyStore.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{
		ExpectedRevision: 1, Enabled: true, Zone: zoneID,
		Origins: []string{"1.1.1.1"}, ProxyMode: "dns-only",
	}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("ambiguous zone = %v", err)
	}
	assertInitial()
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_provider_credentials
		WHERE provider='cloudflare' AND zone_id=$1`, ambiguousID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `CREATE FUNCTION eregion_test_fail_enabled_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'enabled policy audit unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER eregion_test_fail_enabled_audit BEFORE INSERT ON eregion_integration_audit
		FOR EACH ROW EXECUTE FUNCTION eregion_test_fail_enabled_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS eregion_test_fail_enabled_audit ON eregion_integration_audit`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS eregion_test_fail_enabled_audit()`)
	})
	if _, err := policyStore.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{
		ExpectedRevision: 1, Enabled: true, Zone: zoneID,
		Origins: []string{"8.8.4.4"}, ProxyMode: "proxied",
	}); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("enabled audit rejection = %v", err)
	}
	assertInitial()
	if _, err := pool.Exec(ctx, `DROP TRIGGER eregion_test_fail_enabled_audit ON eregion_integration_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION eregion_test_fail_enabled_audit()`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE eregion_definition_dns_policy SET revision=-1 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	if _, err := policyStore.GetDefinitionDNSPolicy(ctx, actor); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("negative policy revision read = %v", err)
	}
	if _, err := policyStore.SetDefinitionDNSPolicy(ctx, actor, api.DefinitionDNSPolicyInput{}); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("negative policy revision write = %v", err)
	}
	var negativeRevision int
	if err := pool.QueryRow(ctx, `SELECT revision FROM eregion_definition_dns_policy WHERE singleton=true`).Scan(&negativeRevision); err != nil || negativeRevision != -1 {
		t.Fatalf("negative policy revision changed to %d, %v", negativeRevision, err)
	}
}
