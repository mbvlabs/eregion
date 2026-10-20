package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationLocalNodeMainAlias covers the database row created for the
// in-process transport. It skips unless EREGION_TEST_DATABASE_URL is set.
func TestIntegrationLocalNodeMainAlias(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()

	main := &Service{pool: pool, localNode: "main"}
	if err := main.ensureLocalNode(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := loadNode(ctx, pool, "local")
	if err != nil {
		t.Fatalf("the local alias did not find the main node: %v", err)
	}
	if record.Name != "main" || !contains(record.Roles, platform.RoleControlPlane) {
		t.Fatalf("unexpected local node: %+v", record)
	}

	// An older deployment that already has a "local" row must keep it rather
	// than gaining a duplicate "main" row.
	pool2, _ := testPool(t, true)
	legacy := &Service{pool: pool2, localNode: "local"}
	if err := legacy.ensureLocalNode(ctx); err != nil {
		t.Fatal(err)
	}
	modern := &Service{pool: pool2, localNode: "main"}
	if err := modern.ensureLocalNode(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool2.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_nodes WHERE name IN ('local','main')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("local/main alias created %d rows", count)
	}
}
