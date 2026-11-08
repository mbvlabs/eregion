package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationDomainAdmissionGuardAndRevalidation(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()

	platformSubmit(t, s, platform.KindNodeRegister, "platform", "guard-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "guard-app", map[string]any{"name": "guard"})
	platformSubmit(t, s, platform.KindEnvironmentCreate, "app/guard", "guard-env", map[string]any{
		"name": "production", "node": "local",
	})
	var envID string
	if err := pool.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name='guard' AND e.name='production'`).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_domains(name,environment_id,is_primary)
        VALUES('app.example.com',$1,false)`, envID); err != nil {
		t.Fatal(err)
	}

	removeInput := map[string]any{"domain": "app.example.com", "confirm": "app.example.com"}
	removeOp := platformSubmit(t, s, platform.KindDomainRemove, "env/guard/production", "guard-remove", removeInput)

	// A readd or promotion cannot be admitted while the removal is pending.
	for _, input := range []map[string]any{
		{"domain": "app.example.com"},
		{"domain": "app.example.com", "primary": true},
	} {
		if _, err := s.Submit(ctx, platformSubmission(platform.KindDomainAdd, "env/guard/production", "guard-readd", input)); contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("readd while removal pending = %v", err)
		}
	}

	op, input, _, err := s.domainOperationInput(ctx, removeOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domains SET is_primary=true WHERE name='app.example.com'`); err != nil {
		t.Fatal(err)
	}
	if err := s.revalidateDomainOperation(ctx, op, input); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("promotion revalidation = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_domains SET is_primary=false WHERE name='app.example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_platform_domains WHERE name='app.example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_domains(name,environment_id,is_primary,created_at)
        VALUES('app.example.com',$1,false,$2)`, envID, op.CreatedAt.Add(1_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := s.revalidateDomainOperation(ctx, op, input); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("readd revalidation = %v", err)
	}
}
