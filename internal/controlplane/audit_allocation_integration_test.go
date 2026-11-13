package controlplane

import (
	"context"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/integrations"
)

// Uses testPool's explicitly authorized isolated database, never a shared DB.
func TestIntegrationAuditConcurrentAllocation(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	const count = 12
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback(ctx)
			if err = integrations.InsertAuditTx(ctx, tx, []byte(`{}`)); err == nil {
				err = tx.Commit(ctx)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n, first, last int64
	if err := pool.QueryRow(ctx, `SELECT count(*),min(id),max(id) FROM eregion_integration_audit`).Scan(&n, &first, &last); err != nil {
		t.Fatal(err)
	}
	if n != count || first != 1 || last != count {
		t.Fatalf("count=%d first=%d last=%d", n, first, last)
	}
}
