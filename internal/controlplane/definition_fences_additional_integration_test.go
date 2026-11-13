package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func TestIntegrationDefinitionNodeFactOrdering(t *testing.T) {
	pool, _, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	roles := []string{platform.RoleApplication, platform.RoleBuilder}
	capabilities := []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET roles=$1,capabilities=$2 WHERE name='local'`, roles, capabilities); err != nil {
		t.Fatal(err)
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET roles=$1,capabilities=$2 WHERE name='local'`,
		[]string{roles[1], roles[0]}, []string{capabilities[1], capabilities[0]}); err != nil {
		t.Fatal(err)
	}
	again, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || again.Digest != plan.Digest {
		t.Fatalf("node fact reorder changed digest: %s err=%v", again.Digest, err)
	}
}

func TestIntegrationDefinitionResourcePlacementSnapshot(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	node := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindNodeRegister, "platform", "resource-node-other", map[string]any{
		"name": "other", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	if err := runPlatformAttempt(ctx, s, node.ID, 1); err != nil {
		t.Fatal(err)
	}
	resource := platformSubmitAs(t, s, definitionIntegrationActor, platform.KindResourceApply, "resource/api/production/db", "snapshot-resource", map[string]any{
		"image": "redis:7", "health": map[string]any{"type": "exec", "command": []string{"true"}},
	})
	// This regression exercises admission and snapshot metadata, not a node
	// effect. Mark the fixture's recorded resource work settled.
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='succeeded' WHERE id=$1`, resource.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='ready' WHERE name='db'`); err != nil {
		t.Fatal(err)
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET node_name='other' WHERE name='db'`); err != nil {
		t.Fatal(err)
	}
	_, err = app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "resource-node-stale", Confirm: "api/production",
	})
	fault := contract.Classify(err)
	if fault.Code != contract.Conflict || !strings.Contains(fault.Hint, "binding/resource/secret metadata") || !strings.Contains(fault.Hint, "new plan") {
		t.Fatalf("resource placement invalidation: %+v", fault)
	}
}

func TestIntegrationDefinitionHostnameClaimAfterAdmissionDrains(t *testing.T) {
	pool, s, app := definitionIntegrationFixture(t)
	ctx := context.Background()
	for _, action := range []struct {
		kind, target, key string
		input             map[string]any
	}{
		{platform.KindAppCreate, "platform", "claim-other-app", map[string]any{"name": "other"}},
		{platform.KindEnvironmentCreate, "app/other", "claim-other-env", map[string]any{"name": "production", "node": "local"}},
	} {
		op := platformSubmitAs(t, s, definitionIntegrationActor, action.kind, action.target, action.key, action.input)
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: integrationDefinition + `
[[environments.production.domains]]
name = "late.example.com"
primary = true
`}
	plan, err := app.PlanDefinition(ctx, definitionIntegrationActor, input)
	if err != nil || !plan.CanApply {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	input.PlanNonce = plan.PlanNonce
	result, err := app.ApplyDefinition(ctx, definitionIntegrationActor, api.DefinitionApplyInput{
		Plan: input, ApprovedDigest: plan.Digest, IdempotencyKey: "late-host-parent", Confirm: "api/production",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := result.Operation.ID
	// Another target may claim an absent name after parent admission.
	platformSubmitAs(t, s, definitionIntegrationActor, platform.KindDomainAdd, "env/other/production", "late-host-claim", map[string]any{"domain": "late.example.com", "primary": true})
	claimed, _, ok, err := s.claimPlatform(ctx, parent, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	worker := &platformWorker{service: s}
	var sawCancelling bool
	for i := 0; i < 16; i++ {
		err := worker.runDefinitionApply(ctx, claimed, 1)
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatalf("parent step: %v", err)
		}
		var state contract.State
		var reserved bool
		if err := pool.QueryRow(ctx, `SELECT o.state,r.finished_at IS NULL FROM eregion_operations o JOIN eregion_definition_runs r ON r.operation_id=o.id WHERE o.id=$1`, parent).Scan(&state, &reserved); err != nil {
			t.Fatal(err)
		}
		if state == contract.Cancelling {
			sawCancelling = true
			if !reserved {
				t.Fatal("reservation cleared before drain")
			}
		}
		if state.Terminal() {
			if state != contract.Failed || reserved || !sawCancelling {
				t.Fatalf("state=%s reserved=%v sawCancelling=%v", state, reserved, sawCancelling)
			}
			break
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
		if i == 15 {
			t.Fatal("parent did not settle")
		}
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT a.name FROM eregion_platform_domains d JOIN eregion_platform_environments e ON e.id=d.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE d.name='late.example.com'`).Scan(&owner); err != nil || owner != "other" {
		t.Fatalf("hostname owner=%q err=%v", owner, err)
	}
}
