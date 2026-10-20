package telemetry

// Test-only access to the unexported production adapters. The external
// integration test lives in package telemetry_test so it can import the
// controlplane migrations without creating an import cycle.
func TablePlacementForTest(pool NodeQueryer) PlacementResolver { return &tablePlacement{pool: pool} }

func TableStatusForTest(pool NodeQueryer) StatusProvider { return newTableStatus(pool, nil) }
