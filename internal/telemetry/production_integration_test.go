package telemetry_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type noopTransport struct{ name string }

func (n *noopTransport) Name() string         { return "noop" }
func (n *noopTransport) NodeIdentity() string { return n.name }
func (n *noopTransport) Call(context.Context, node.Request, func(node.Event)) (node.Response, error) {
	return node.Response{}, errors.New("noop transport")
}

// TestIntegrationProductionPlacement uses the real migration and table shape.
// It is skipped unless EREGION_TEST_DATABASE_URL points at a PostgreSQL server
// where the test can create an isolated database.
func TestIntegrationProductionPlacement(t *testing.T) {
	dsn := os.Getenv("EREGION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run the production placement integration test")
	}
	ctx := context.Background()
	admin, err := controlplane.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	name := "eregion_telemetry_" + strings.TrimPrefix(string(id), "op_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed, _ := url.Parse(dsn)
	parsed.Path = "/" + name
	pool, err := controlplane.Open(ctx, parsed.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	if _, err := controlplane.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes (name,roles,capabilities,address)
        VALUES ('telemetry-1',ARRAY['telemetry'],ARRAY['docker'],'10.77.0.10:22')`); err != nil {
		t.Fatal(err)
	}
	placement, err := telemetry.TablePlacementForTest(pool).ResolveNode(ctx, "telemetry-1")
	if err != nil {
		t.Fatal(err)
	}
	if placement.PrivateAddress != "10.77.0.10" || placement.IsControlPlane || placement.IsMain {
		t.Fatalf("unexpected placement from the real table: %+v", placement)
	}
	status, err := telemetry.TableStatusForTest(pool).TelemetryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.State != telemetry.StatusUnknown || status.Endpoints["logs"] != "10.77.0.10:9427" {
		t.Fatalf("unexpected status from the real table: %+v", status)
	}
	if err := telemetry.ConfigureProduction(platform.NewRegistry(), pool, &noopTransport{name: "telemetry-1"}); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes (name,roles,capabilities,address)
        VALUES ('main',ARRAY['control-plane','telemetry'],ARRAY['docker'],'10.77.0.1:22')`); err != nil {
		t.Fatal(err)
	}
	if _, err := telemetry.TablePlacementForTest(pool).ResolveNode(ctx, "main"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("telemetry on the control plane was not refused: %v", err)
	}
}
