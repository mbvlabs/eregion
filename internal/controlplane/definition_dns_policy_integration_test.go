package controlplane

import (
	"context"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
)

// testPool creates and drops an isolated database; run only against an
// explicitly authorized EREGION_TEST_DATABASE_URL.
func TestIntegrationDefinitionDNSPolicyAbsentCASAndReadError(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	store := NewDefinitionDNSPolicyStore(pool)
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
