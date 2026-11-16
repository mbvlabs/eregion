package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
)

// These tests use testPool's dedicated database. They must be run only against
// an explicitly authorized EREGION_TEST_DATABASE_URL.
func definitionWithoutPolicy(t *testing.T) (*pgxpool.Pool, *Service, api.AppService, api.DefinitionApplyInput) {
	t.Helper()
	pool, service, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	// The historical migration seeded this row; the fresh baseline does not.
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_definition_dns_policy`); err != nil {
		t.Fatal(err)
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("preview: %+v, %v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	return pool, service, app, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, Confirm: "api/production", IdempotencyKey: "absent-policy",
	}
}

func TestIntegrationDefinitionApplyWithoutPolicy(t *testing.T) {
	pool, _, app, input := definitionWithoutPolicy(t)
	result, err := app.ApplyDefinition(context.Background(), definitionIntegrationActor, input)
	if err != nil || result.Replay || result.Operation.State != contract.Queued {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_definition_dns_policy`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("apply created a policy row: count=%d err=%v", count, err)
	}
}

func TestIntegrationDefinitionApplyPolicyQueryError(t *testing.T) {
	pool, _, app, input := definitionWithoutPolicy(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE eregion_definition_dns_policy RENAME TO unavailable_definition_dns_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ApplyDefinition(ctx, definitionIntegrationActor, input); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing table: %v", err)
	}
}

func TestIntegrationDefinitionApplyConcurrentPolicyChange(t *testing.T) {
	pool, service, app, input := definitionWithoutPolicy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	guard, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Rollback(context.Background())
	if _, err := guard.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, definitionPolicyLock); err != nil {
		t.Fatal(err)
	}
	policyResult := make(chan error, 1)
	go func() {
		_, err := NewDefinitionDNSPolicyStore(service.entities).SetDefinitionDNSPolicy(ctx, definitionIntegrationActor, api.DefinitionDNSPolicyInput{})
		policyResult <- err
	}()
	waitForPolicyWaiters(t, ctx, pool, 1)
	applyResult := make(chan error, 1)
	go func() {
		_, err := app.ApplyDefinition(ctx, definitionIntegrationActor, input)
		applyResult <- err
	}()
	waitForPolicyWaiters(t, ctx, pool, 2)
	if err := guard.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-policyResult; err != nil {
		t.Fatalf("policy change: %v", err)
	}
	if err := <-applyResult; contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("stale approved plan: %v", err)
	}
}

func waitForPolicyWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, count int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'
			  AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d policy lock waiters: %v", count, ctx.Err())
		case <-ticker.C:
		}
	}
}
