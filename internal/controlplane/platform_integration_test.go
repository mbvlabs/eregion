package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func platformSubmission(kind, target, key string, input any) operations.Submission {
	raw, _ := json.Marshal(input)
	return operations.Submission{Kind: kind, Target: target, Actor: testActor, IdempotencyKey: key, Input: raw}
}

func platformSubmit(t *testing.T, s *Service, kind, target, key string, input any) contract.Operation {
	t.Helper()
	op, err := s.Submit(context.Background(), platformSubmission(kind, target, key, input))
	if err != nil {
		t.Fatalf("submit %s: %v (%v)", kind, err, errors.Unwrap(err))
	}
	return op
}

func platformAwait(t *testing.T, s *Service, op contract.Operation, want contract.State) contract.Operation {
	t.Helper()
	await(t, 60*time.Second, func() bool {
		current, err := s.Get(context.Background(), testActor, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		op = current
		return op.State.Terminal()
	})
	if op.State != want {
		page, _ := s.Events(context.Background(), testActor, contract.Cursor{OperationID: op.ID}, 200)
		raw, _ := json.Marshal(page)
		t.Fatalf("%s: got %s, want %s; events=%s", op.Kind, op.State, want, raw)
	}
	return op
}

func TestIntegrationPlatformAtomicAndTargetLock(t *testing.T) {
	pool, dsn := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	original := run.Service.enqueuePlatform
	run.Service.enqueuePlatform = func(ctx context.Context, tx pgx.Tx, op contract.Operation) (int64, error) {
		_, err := original(ctx, tx, op)
		if err != nil {
			return 0, err
		}
		return 0, errors.New("injected failure after job insertion")
	}
	_, err := run.Service.Submit(ctx, platformSubmission(platform.KindAppCreate, "platform", "atomic", map[string]any{"name": "atomic"}))
	if err == nil {
		t.Fatal("partial submission succeeded")
	}
	for _, table := range []string{"eregion_operations", "eregion_platform_apps", "eregion_operation_events", "river_job"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s count=%d err=%v", table, count, err)
		}
	}
	otherPool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	other := &Service{pool: otherPool}
	for _, target := range []string{"env/live/production", "node/local", "platform"} {
		release, err := run.Service.lockTarget(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		unlock, err := other.lockTarget(blocked, target)
		cancel()
		if err == nil {
			unlock()
			release()
			t.Fatalf("target %s was not serialized across sessions", target)
		}
		release()
		unlock, err = other.lockTarget(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
}
