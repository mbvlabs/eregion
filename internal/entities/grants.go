package entities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/integrations"
)

// GrantStore is the typed persistence capability for private-device access
// grants. It borrows the Store's pool and exposes only the integrations
// GrantStore port: no transaction, generated query or driver handle leaves this
// package.
type GrantStore struct {
	store *Store
	q     *query.Queries
}

// Grants returns the access-grant persistence view of this Store.
func (s *Store) Grants() *GrantStore {
	return &GrantStore{store: s, q: query.New(s.pool)}
}

// The access controller depends on this exact port; a missing or renamed method
// fails the build instead of reaching production as a runtime assertion.
var _ integrations.GrantStore = (*GrantStore)(nil)

// Get returns one grant and whether it exists. A missing row is (zero, false,
// nil); every other failure surfaces as the access-store unavailability fault.
func (g *GrantStore) Get(ctx context.Context, id string) (integrations.DeviceGrant, bool, error) {
	raw, err := g.q.GetAccessGrantRecord(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.DeviceGrant{}, false, nil
	}
	if err != nil {
		return integrations.DeviceGrant{}, false, integrations.GrantStoreUnavailable(err)
	}
	grant, err := decodeGrantRecord(raw)
	if err != nil {
		return grant, false, integrations.GrantStoreUnavailable(err)
	}
	return grant, true, nil
}

// List returns every grant ordered by identity. It applies no subject, device or
// route filter in the database; filtering and presentation remain the access
// controller's responsibility.
func (g *GrantStore) List(ctx context.Context) ([]integrations.DeviceGrant, error) {
	records, err := g.q.ListAccessGrantRecords(ctx)
	if err != nil {
		return nil, integrations.GrantStoreUnavailable(err)
	}
	result := make([]integrations.DeviceGrant, 0, len(records))
	for _, raw := range records {
		grant, err := decodeGrantRecord(raw)
		if err != nil {
			return nil, integrations.GrantStoreUnavailable(err)
		}
		result = append(result, grant)
	}
	return result, nil
}

// Put persists a grant without replacing an existing creation request and
// without reviving a revoked grant. A revocation is written only when the stored
// grant is not yet revoked and the request carries one; the row lock serializes
// competing writers, so the first revocation wins and later ones leave it
// untouched.
func (g *GrantStore) Put(ctx context.Context, grant integrations.DeviceGrant) error {
	if err := integrations.ValidateGrantID(grant.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(grant)
	if err != nil {
		return integrations.GrantStoreUnavailable(err)
	}
	err = g.store.WithinTx(ctx, func(tx *StoreTx) error {
		if err := tx.q.InsertAccessGrantIfAbsent(ctx, query.InsertAccessGrantIfAbsentParams{ID: grant.ID, Record: raw}); err != nil {
			return integrations.GrantStoreUnavailable(err)
		}
		existingRaw, err := tx.q.LockAccessGrantRecord(ctx, grant.ID)
		if err != nil {
			return integrations.GrantStoreUnavailable(err)
		}
		existing, err := decodeGrantRecord(existingRaw)
		if err != nil {
			return integrations.GrantStoreUnavailable(err)
		}
		if !integrations.GrantReplayMatches(existing, grant) {
			return contract.NewFault(contract.Conflict, "grant identity already binds different input", "Retry the original operation unchanged.")
		}
		if existing.RevokedAt == nil && grant.RevokedAt != nil {
			existing.RevokedAt, existing.RevokedBy, existing.RevokedByOperation = grant.RevokedAt, grant.RevokedBy, grant.RevokedByOperation
			revoked, err := json.Marshal(existing)
			if err != nil {
				return integrations.GrantStoreUnavailable(err)
			}
			if err := tx.q.UpdateAccessGrantRecord(ctx, query.UpdateAccessGrantRecordParams{ID: grant.ID, Record: revoked}); err != nil {
				return integrations.GrantStoreUnavailable(err)
			}
		}
		return nil
	})
	if err != nil {
		return integrations.GrantStoreUnavailable(err)
	}
	return nil
}

// decodeGrantRecord maps the private generated row to the integrations model by
// JSON, the record format the schema stores. Fields absent from the record stay
// at their zero value; RevokedAt stays nil unless a revocation was persisted.
func decodeGrantRecord(raw json.RawMessage) (integrations.DeviceGrant, error) {
	var grant integrations.DeviceGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return grant, err
	}
	return grant, nil
}
