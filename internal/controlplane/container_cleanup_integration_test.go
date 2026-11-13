package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationContainerCleanupRows proves the durable retry record: a
// repeated failure increments attempts and delays the next due time, and the
// row cascades with its environment.
func TestIntegrationContainerCleanupRows(t *testing.T) {
	pool, _ := testPool(t, true)
	service := &Service{pool: pool}
	ctx := context.Background()

	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureNode(ctx, pool, "main", []string{"application"}, []string{"docker"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, string(appID), "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "main", 20001, 30001, 8080); err != nil {
		t.Fatal(err)
	}

	container := "eregion-demo-production-web-1"
	failure := contract.NewFault(contract.PreconditionFailed, "docker rm failed", "Inspect the container.")
	if err := service.recordContainerCleanup(ctx, container, envID, "main", "", "previous", failure); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var delaySeconds float64
	if err := pool.QueryRow(ctx, `SELECT attempts,extract(epoch FROM (next_attempt_at-created_at))
        FROM eregion_platform_container_cleanup WHERE container_name=$1`, container).Scan(&attempts, &delaySeconds); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || delaySeconds < 30 || delaySeconds > 90 {
		t.Fatalf("first cleanup row attempts=%d delay=%fs", attempts, delaySeconds)
	}
	if err := service.recordContainerCleanup(ctx, container, envID, "main", "", "previous", failure); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempts FROM eregion_platform_container_cleanup WHERE container_name=$1`, container).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("repeated failure attempts=%d, want 2", attempts)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_platform_environments WHERE id=$1`, envID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_container_cleanup WHERE container_name=$1`, container).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("cleanup row survived its environment: %d", remaining)
	}
}
