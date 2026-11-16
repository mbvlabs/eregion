package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
)

// testGrant builds a canonical creation request for the typed store. The ID and
// operation identity are fixed so concurrent and replay assertions are exact.
func testGrant(seed string, now time.Time) integrations.DeviceGrant {
	return integrations.DeviceGrant{
		ID:                 "grant_" + strings.Repeat(seed, 32),
		Subject:            "alice",
		DeviceID:           "alpha",
		Routes:             []string{"10.0.0.0/8", "app.example"},
		CreatedBy:          "operator",
		CreatedByOperation: "op_" + strings.Repeat(seed, 32),
		CreatedAt:          now,
		ExpiresAt:          now.Add(2 * time.Hour),
		Note:               "typed store contract",
	}
}

// The typed store preserves the deleted PostgreSQL store's whole contract:
// not-found as (zero, false, nil), unbounded identity order, insert-if-absent,
// replay tolerance for a defaulted expiry, conflict for different input, and a
// row-locked revocation that neither revives nor replaces the first revocation.
func TestIntegrationEntitiesGrantStoreContract(t *testing.T) {
	pool, _ := testPool(t, true)
	grants := newTestEntities(t, pool).Grants()
	ctx := context.Background()
	now := time.Now().UTC()
	grant := testGrant("a", now)

	if list, err := grants.List(ctx); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v; want a non-nil empty snapshot", list, err)
	}
	if got, found, err := grants.Get(ctx, grant.ID); err != nil || found || got.ID != "" {
		t.Fatalf("missing get = %+v, %v, %v; want zero, false, nil", got, found, err)
	}

	var validation *contract.Fault
	if err := grants.Put(ctx, integrations.DeviceGrant{ID: "grant_short"}); !errors.As(err, &validation) || validation.Code != contract.ValidationFailed {
		t.Fatalf("invalid identity put = %v; want validation fault", err)
	}

	if err := grants.Put(ctx, grant); err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, found, err := grants.Get(ctx, grant.ID)
	if err != nil || !found {
		t.Fatalf("get after create = %+v, %v, %v", stored, found, err)
	}
	if stored.Subject != grant.Subject || stored.DeviceID != grant.DeviceID || stored.CreatedBy != grant.CreatedBy ||
		stored.CreatedByOperation != grant.CreatedByOperation || stored.Note != grant.Note ||
		!stored.CreatedAt.Equal(grant.CreatedAt) || !stored.ExpiresAt.Equal(grant.ExpiresAt) ||
		stored.RevokedAt != nil || stored.RevokedBy != "" || stored.RevokedByOperation != "" {
		t.Fatalf("stored creation grant changed: %+v", stored)
	}
	if len(stored.Routes) != 2 || stored.Routes[0] != grant.Routes[0] || stored.Routes[1] != grant.Routes[1] {
		t.Fatalf("stored routes = %v, want %v", stored.Routes, grant.Routes)
	}

	// A creation replay never replaces the original record. The store Put path
	// cannot know whether the caller defaulted the expiry, so it accepts a
	// different explicit expiry as the same request and keeps the first record.
	replay := grant
	replay.ExpiresAt = now.Add(48 * time.Hour)
	if err := grants.Put(ctx, replay); err != nil {
		t.Fatalf("creation replay: %v", err)
	}
	stored, _, err = grants.Get(ctx, grant.ID)
	if err != nil || !stored.ExpiresAt.Equal(grant.ExpiresAt) {
		t.Fatalf("creation replay replaced expiry: %+v, %v", stored, err)
	}

	conflicting := grant
	conflicting.Subject = "bob"
	var conflict *contract.Fault
	if err := grants.Put(ctx, conflicting); !errors.As(err, &conflict) || conflict.Code != contract.Conflict {
		t.Fatalf("conflicting put = %v; want conflict fault", err)
	}
	if stored, _, err := grants.Get(ctx, grant.ID); err != nil || stored.Subject != grant.Subject {
		t.Fatalf("conflict changed stored grant: %+v, %v", stored, err)
	}

	revokedAt := now.Add(time.Minute)
	revocation := grant
	revocation.RevokedAt = &revokedAt
	revocation.RevokedBy = "operator"
	revocation.RevokedByOperation = "op_revoke_first"
	if err := grants.Put(ctx, revocation); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	stored, _, err = grants.Get(ctx, grant.ID)
	if err != nil || stored.RevokedAt == nil || !stored.RevokedAt.Equal(revokedAt) ||
		stored.RevokedBy != "operator" || stored.RevokedByOperation != "op_revoke_first" {
		t.Fatalf("revocation not persisted exactly: %+v, %v", stored, err)
	}

	// Retrying the creation request after a revocation must not revive the grant.
	if err := grants.Put(ctx, grant); err != nil {
		t.Fatalf("creation replay after revoke: %v", err)
	}
	stored, _, err = grants.Get(ctx, grant.ID)
	if err != nil || stored.RevokedAt == nil || !stored.RevokedAt.Equal(revokedAt) {
		t.Fatalf("creation replay revived the grant: %+v, %v", stored, err)
	}

	// A later revocation cannot replace the first one.
	later := now.Add(2 * time.Minute)
	competing := grant
	competing.RevokedAt = &later
	competing.RevokedBy = "other"
	competing.RevokedByOperation = "op_revoke_second"
	if err := grants.Put(ctx, competing); err != nil {
		t.Fatalf("competing revocation: %v", err)
	}
	stored, _, err = grants.Get(ctx, grant.ID)
	if err != nil || !stored.RevokedAt.Equal(revokedAt) || stored.RevokedBy != "operator" || stored.RevokedByOperation != "op_revoke_first" {
		t.Fatalf("competing revocation replaced the first: %+v, %v", stored, err)
	}

	// List is unbounded and ordered by grant identity only.
	second := testGrant("b", now)
	second.Subject, second.DeviceID, second.Note = "bob", "beta", "second"
	if err := grants.Put(ctx, second); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	list, err := grants.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != grant.ID || list[1].ID != second.ID {
		t.Fatalf("ordered list = %+v, %v", list, err)
	}

	// A stored record that is not a grant object is an unavailable store, not an
	// access decision, on both point reads and scans.
	malformedID := "grant_" + strings.Repeat("c", 32)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_access_grants (id, record, created_at) VALUES ($1, $2::jsonb, clock_timestamp())`, malformedID, `"not-a-grant"`); err != nil {
		t.Fatal(err)
	}
	var unavailable *contract.Fault
	if _, _, err := grants.Get(ctx, malformedID); !errors.As(err, &unavailable) || unavailable.Code != contract.Unavailable {
		t.Fatalf("malformed get = %v; want unavailable fault", err)
	}
	unavailable = nil
	if _, err := grants.List(ctx); !errors.As(err, &unavailable) || unavailable.Code != contract.Unavailable {
		t.Fatalf("malformed list = %v; want unavailable fault", err)
	}

	// A canceled caller context fails as unavailable and leaves no row.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	unavailable = nil
	unwritten := testGrant("d", now)
	if err := grants.Put(canceled, unwritten); !errors.As(err, &unavailable) || unavailable.Code != contract.Unavailable {
		t.Fatalf("canceled put = %v; want unavailable fault", err)
	}
	if _, found, err := grants.Get(ctx, unwritten.ID); err != nil || found {
		t.Fatalf("canceled put persisted a row: %v, %v", found, err)
	}
}

// Concurrent creation replays all succeed against one row, and competing
// revocations serialize on the row lock so exactly the first revocation is
// durable and no later writer replaces it.
func TestIntegrationEntitiesGrantStoreConcurrentWriters(t *testing.T) {
	pool, _ := testPool(t, true)
	grants := newTestEntities(t, pool).Grants()
	ctx := context.Background()
	now := time.Now().UTC()
	grant := testGrant("e", now)

	const writers = 8
	var create sync.WaitGroup
	created := make(chan error, writers)
	for index := 0; index < writers; index++ {
		create.Add(1)
		go func() {
			defer create.Done()
			created <- grants.Put(ctx, grant)
		}()
	}
	create.Wait()
	close(created)
	for err := range created {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	if list, err := grants.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("concurrent create rows = %d, %v; want 1", len(list), err)
	}

	var revoke sync.WaitGroup
	revoked := make(chan error, writers)
	for index := 0; index < writers; index++ {
		revoke.Add(1)
		go func(index int) {
			defer revoke.Done()
			at := now.Add(time.Duration(index+1) * time.Second)
			request := grant
			request.RevokedAt = &at
			request.RevokedBy = fmt.Sprintf("operator-%d", index)
			request.RevokedByOperation = fmt.Sprintf("op_revoke_%d", index)
			revoked <- grants.Put(ctx, request)
		}(index)
	}
	revoke.Wait()
	close(revoked)
	for err := range revoked {
		if err != nil {
			t.Fatalf("concurrent revoke: %v", err)
		}
	}
	stored, found, err := grants.Get(ctx, grant.ID)
	if err != nil || !found || stored.RevokedAt == nil || !strings.HasPrefix(stored.RevokedBy, "operator-") {
		t.Fatalf("concurrent revoke result = %+v, %v, %v", stored, found, err)
	}
	first := *stored.RevokedAt
	again, _, err := grants.Get(ctx, grant.ID)
	if err != nil || again.RevokedAt == nil || !again.RevokedAt.Equal(first) || again.RevokedBy != stored.RevokedBy {
		t.Fatalf("revocation was replaced after the race: %+v, %v", again, err)
	}
}
