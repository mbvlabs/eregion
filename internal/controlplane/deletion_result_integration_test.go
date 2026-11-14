package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationDomainRemovalRetainsOriginalBindingResult(t *testing.T) {
	pool, service, _ := definitionIntegrationFixture(t)
	ctx := context.Background()
	environment, err := service.environment(ctx, "api", "production")
	if err != nil {
		t.Fatal(err)
	}
	const domain = "removed.example.test"
	insertFixtureDomain(t, ctx, pool, domain, string(environment.ID), false, time.Now())
	var originalID platform.DomainID
	if err := pool.QueryRow(ctx, `SELECT id FROM eregion_platform_domains WHERE name=$1`, domain).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	service.routeSync = func(context.Context) error { return nil }
	observed := 0
	service.observeActionAttempt = func(kind, outcome string, duration time.Duration) {
		if kind == platform.KindDomainRemove {
			if outcome != "succeeded" || duration < 0 {
				t.Errorf("unexpected action observation: %s %s %v", kind, outcome, duration)
			}
			observed++
		}
	}
	op := platformSubmitAs(t, service, definitionIntegrationActor, platform.KindDomainRemove,
		"env/api/production", "retained-domain-removal", map[string]any{"domain": domain, "confirm": domain})
	if err := runPlatformAttempt(ctx, service, op.ID, 1); err != nil {
		t.Fatal(err)
	}
	completed, err := service.Get(ctx, definitionIntegrationActor, op.ID)
	if err != nil || completed.State != contract.Succeeded || completed.LatestResult == nil {
		t.Fatalf("domain result: %+v, %v", completed, err)
	}
	var result struct {
		DomainID              platform.DomainID `json:"domain_id"`
		Domain                string            `json:"domain"`
		DesiredBindingRemoved bool              `json:"desired_binding_removed"`
		RouteSync             string            `json:"route_sync"`
	}
	if err := json.Unmarshal(completed.LatestResult.Data, &result); err != nil ||
		result.DomainID != originalID || result.Domain != domain || !result.DesiredBindingRemoved ||
		result.RouteSync != "not_observed" || completed.LatestResult.DataCursor == nil {
		t.Fatalf("original binding snapshot missing or overclaims route observation: %+v, %v", result, err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_domains WHERE id=$1`, originalID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("binding was not removed: %d, %v", remaining, err)
	}
	page, err := service.ListOperations(ctx, definitionIntegrationActor, operations.ListQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == op.ID {
			if item.LatestResult == nil || string(item.LatestResult.Data) != string(completed.LatestResult.Data) || observed != 1 {
				t.Fatalf("list result or action observation differs: %+v, observations=%d", item, observed)
			}
			return
		}
	}
	t.Fatal("removed binding operation missing from history")
}
