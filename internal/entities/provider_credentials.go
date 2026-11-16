package entities

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/vault"
)

// ProviderStore is the read-only provider-credential persistence capability.
// It borrows the Store's pool and exposes only the vault.ProviderReader port.
type ProviderStore struct {
	q *query.Queries
}

// Providers returns the provider-credential read view of this Store.
func (s *Store) Providers() *ProviderStore { return &ProviderStore{q: query.New(s.pool)} }

var _ vault.ProviderReader = (*ProviderStore)(nil)

func (p *ProviderStore) ProviderMetadata(ctx context.Context, scope vault.ProviderScope) (vault.ProviderMetadata, bool, error) {
	return providerMetadataQuery(ctx, p.q, scope)
}

func (p *ProviderStore) ListProviderMetadata(ctx context.Context, provider string, limit int) ([]vault.ProviderMetadata, error) {
	return listProviderMetadataQuery(ctx, p.q, provider, limit)
}

func (p *ProviderStore) ProviderEncryptedRevision(ctx context.Context, scope vault.ProviderScope) (vault.ProviderEncryptedRevision, bool, error) {
	return providerEncryptedQuery(ctx, p.q, scope)
}

// ProviderTx is the provider-credential view of one StoreTx. It implements the
// vault's narrow transaction port; every method uses the caller's transaction.
type ProviderTx struct{ owner *StoreTx }

// Providers returns the provider-credential view of this transaction.
func (tx *StoreTx) Providers() *ProviderTx { return &ProviderTx{owner: tx} }

var _ vault.ProviderTx = (*ProviderTx)(nil)

func (p *ProviderTx) ProviderMetadata(ctx context.Context, scope vault.ProviderScope) (vault.ProviderMetadata, bool, error) {
	return providerMetadataQuery(ctx, p.owner.q, scope)
}

// ListProviderMetadata returns secret-free metadata inside the caller's
// transaction. It is the transaction-bound form used when admission must
// resolve a zone without borrowing a second pool connection.
func (p *ProviderTx) ListProviderMetadata(ctx context.Context, provider string, limit int) ([]vault.ProviderMetadata, error) {
	return listProviderMetadataQuery(ctx, p.owner.q, provider, limit)
}

// LockProviderCredential locks the exact scope row and returns its current
// revision and zone name. A missing row is (zero, false, nil).
func (p *ProviderTx) LockProviderCredential(ctx context.Context, scope vault.ProviderScope) (vault.ProviderCredentialState, bool, error) {
	row, err := p.owner.q.LockProviderCredential(ctx, query.LockProviderCredentialParams{
		Provider: scope.Provider, ZoneID: scope.ZoneID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.ProviderCredentialState{}, false, nil
	}
	if err != nil {
		return vault.ProviderCredentialState{}, false, err
	}
	latest, err := providerRevisionValue(row.LatestRevision)
	if err != nil {
		return vault.ProviderCredentialState{}, false, err
	}
	return vault.ProviderCredentialState{LatestRevision: latest, ZoneName: row.ZoneName}, true, nil
}

// InsertProviderCredential creates revision zero state. It returns false when a
// zone ID or zone name already exists; entities classifies the unique violation
// so the vault provider domain never imports a driver to do so.
func (p *ProviderTx) InsertProviderCredential(ctx context.Context, input vault.ProviderCredentialInput) (bool, error) {
	err := p.owner.q.InsertProviderCredential(ctx, query.InsertProviderCredentialParams{
		Provider: input.Scope.Provider, ZoneID: input.Scope.ZoneID, ZoneName: input.ZoneName,
		ActorKind: input.Actor.Kind, ActorSubject: input.Actor.Subject,
	})
	if isProviderUniqueViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpdateProviderCredentialRevision applies the locked CAS pointer update. It
// returns false when the expected revision no longer matches.
func (p *ProviderTx) UpdateProviderCredentialRevision(ctx context.Context, scope vault.ProviderScope, expectedRevision, revision uint64, actor contract.Actor) (bool, error) {
	expected, err := providerBigintValue(expectedRevision)
	if err != nil {
		return false, err
	}
	next, err := providerBigintValue(revision)
	if err != nil {
		return false, err
	}
	affected, err := p.owner.q.UpdateProviderCredentialRevision(ctx, query.UpdateProviderCredentialRevisionParams{
		Revision: next, ActorKind: actor.Kind, ActorSubject: actor.Subject,
		Provider: scope.Provider, ZoneID: scope.ZoneID, ExpectedRevision: expected,
	})
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// InsertProviderCredentialRevision appends one immutable encrypted revision. It
// returns false when the revision already exists.
func (p *ProviderTx) InsertProviderCredentialRevision(ctx context.Context, input vault.ProviderRevisionInput) (bool, error) {
	revision, err := providerBigintValue(input.Revision)
	if err != nil {
		return false, err
	}
	err = p.owner.q.InsertProviderCredentialRevision(ctx, query.InsertProviderCredentialRevisionParams{
		Provider: input.Scope.Provider, ZoneID: input.Scope.ZoneID, Revision: revision,
		KeyID: input.KeyID, Ciphertext: input.Ciphertext, CiphertextSha256: input.CiphertextSHA256,
		RevisionHash: input.RevisionHash, ActorKind: input.Actor.Kind, ActorSubject: input.Actor.Subject,
	})
	if isProviderUniqueViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func providerMetadataQuery(ctx context.Context, q *query.Queries, scope vault.ProviderScope) (vault.ProviderMetadata, bool, error) {
	row, err := q.GetProviderCredentialMetadata(ctx, query.GetProviderCredentialMetadataParams{
		Provider: scope.Provider, ZoneID: scope.ZoneID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.ProviderMetadata{}, false, nil
	}
	if err != nil {
		return vault.ProviderMetadata{}, false, err
	}
	metadata, err := providerMetadataFromGet(row)
	if err != nil {
		return vault.ProviderMetadata{}, false, err
	}
	return metadata, true, nil
}

func listProviderMetadataQuery(ctx context.Context, q *query.Queries, provider string, limit int) ([]vault.ProviderMetadata, error) {
	rows, err := q.ListProviderCredentialMetadata(ctx, query.ListProviderCredentialMetadataParams{
		Provider: provider, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	metadata := make([]vault.ProviderMetadata, 0, len(rows))
	for _, row := range rows {
		item, err := providerMetadataFromList(row)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, item)
	}
	return metadata, nil
}

func providerEncryptedQuery(ctx context.Context, q *query.Queries, scope vault.ProviderScope) (vault.ProviderEncryptedRevision, bool, error) {
	row, err := q.GetProviderCredentialEncrypted(ctx, query.GetProviderCredentialEncryptedParams{
		Provider: scope.Provider, ZoneID: scope.ZoneID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.ProviderEncryptedRevision{}, false, nil
	}
	if err != nil {
		return vault.ProviderEncryptedRevision{}, false, err
	}
	latest, err := providerRevisionValue(row.LatestRevision)
	if err != nil {
		return vault.ProviderEncryptedRevision{}, false, err
	}
	revision, err := providerRevisionValue(row.Revision)
	if err != nil {
		return vault.ProviderEncryptedRevision{}, false, err
	}
	createdAt, err := providerTimestamp(row.CreatedAt)
	if err != nil {
		return vault.ProviderEncryptedRevision{}, false, err
	}
	return vault.ProviderEncryptedRevision{
		Provider: row.Provider, ZoneID: row.ZoneID, ZoneName: row.ZoneName,
		LatestRevision: latest, Revision: revision, KeyID: row.KeyID,
		Ciphertext: row.Ciphertext, CiphertextSHA256: row.CiphertextSha256,
		RevisionHash: row.RevisionHash, CreatedAt: createdAt,
	}, true, nil
}

func providerMetadataFromGet(row query.GetProviderCredentialMetadataRow) (vault.ProviderMetadata, error) {
	return providerMetadata(row.Provider, row.ZoneID, row.ZoneName, row.LatestRevision,
		row.CreatedByKind, row.CreatedBySubject, row.UpdatedByKind, row.UpdatedBySubject,
		row.CreatedAt, row.UpdatedAt, row.Revision, row.KeyID, row.RevisionHash, row.RevisionCreatedAt)
}

func providerMetadataFromList(row query.ListProviderCredentialMetadataRow) (vault.ProviderMetadata, error) {
	return providerMetadata(row.Provider, row.ZoneID, row.ZoneName, row.LatestRevision,
		row.CreatedByKind, row.CreatedBySubject, row.UpdatedByKind, row.UpdatedBySubject,
		row.CreatedAt, row.UpdatedAt, row.Revision, row.KeyID, row.RevisionHash, row.RevisionCreatedAt)
}

func providerMetadata(provider, zoneID, zoneName string, latestRevision int64,
	createdByKind, createdBySubject, updatedByKind, updatedBySubject string,
	createdAt, updatedAt pgtype.Timestamptz, revision int64, keyID, revisionHash string,
	revisionCreatedAt pgtype.Timestamptz,
) (vault.ProviderMetadata, error) {
	latest, err := providerRevisionValue(latestRevision)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	current, err := providerRevisionValue(revision)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	created, err := providerTimestamp(createdAt)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	updated, err := providerTimestamp(updatedAt)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	revisionCreated, err := providerTimestamp(revisionCreatedAt)
	if err != nil {
		return vault.ProviderMetadata{}, err
	}
	return vault.ProviderMetadata{
		Provider: provider, ZoneID: zoneID, ZoneName: zoneName, LatestRevision: latest,
		Revision:  vault.RevisionRef{Revision: current, KeyID: keyID, Hash: revisionHash, CreatedAt: revisionCreated},
		CreatedBy: contract.Actor{Kind: createdByKind, Subject: createdBySubject},
		UpdatedBy: contract.Actor{Kind: updatedByKind, Subject: updatedBySubject},
		CreatedAt: created, UpdatedAt: updated,
	}, nil
}

// providerRevisionValue converts a persisted bigint revision without wrapping.
// Revisions are positive; a stored zero or negative value is corrupt state.
func providerRevisionValue(value int64) (uint64, error) {
	if value <= 0 {
		return 0, providerCorrupt("provider credential revision is outside the supported range")
	}
	return uint64(value), nil
}

// providerBigintValue bounds a caller revision before it reaches a bigint
// parameter. The vault CAS check is the primary guard; this is defense in depth.
func providerBigintValue(value uint64) (int64, error) {
	if value > math.MaxInt64 {
		return 0, contract.NewFault(contract.Conflict, "provider credential revision is outside the database range", "Re-read the provider status; revision numbers cannot exceed the signed bigint range.")
	}
	return int64(value), nil
}

func providerTimestamp(value pgtype.Timestamptz) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, providerCorrupt("provider credential metadata is incomplete")
	}
	return value.Time.UTC(), nil
}

func providerCorrupt(message string) *contract.FaultError {
	return contract.NewFault(contract.Unavailable, message, "Restore the provider credential from backup; do not infer state from a malformed row.")
}

func isProviderUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ProviderZoneRevision is one non-secret zone revision fact.
type ProviderZoneRevision struct {
	ZoneID   string
	Revision uint64
}

// RevisionForZone reads the latest non-secret revision for one zone.
func (p *ProviderTx) RevisionForZone(ctx context.Context, provider, zoneID string) (uint64, bool, error) {
	revision, err := p.owner.q.GetProviderRevisionForZone(ctx, query.GetProviderRevisionForZoneParams{Provider: provider, ZoneID: zoneID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, err := providerRevisionValue(revision)
	return value, err == nil, err
}

// ZoneRevisionsForDefinition lists the policy zone and current environment
// zone revisions in one read.
func (p *ProviderTx) ZoneRevisionsForDefinition(ctx context.Context, provider, zoneID, environmentID string) ([]ProviderZoneRevision, error) {
	rows, err := p.owner.q.ListProviderZoneRevisionsForDefinition(ctx, query.ListProviderZoneRevisionsForDefinitionParams{
		Provider: provider, ZoneID: zoneID, EnvironmentID: environmentID,
	})
	if err != nil {
		return nil, err
	}
	result := make([]ProviderZoneRevision, 0, len(rows))
	for _, row := range rows {
		value, err := providerRevisionValue(row.LatestRevision)
		if err != nil {
			return nil, err
		}
		result = append(result, ProviderZoneRevision{ZoneID: row.ZoneID, Revision: value})
	}
	return result, nil
}

// LockZoneRevisionsForDefinition share-locks the exact definition zones.
func (p *ProviderTx) LockZoneRevisionsForDefinition(ctx context.Context, provider, zoneID, environmentID string) ([]string, error) {
	return p.owner.q.LockProviderZoneRevisionsForDefinition(ctx, query.LockProviderZoneRevisionsForDefinitionParams{
		Provider: provider, ZoneID: zoneID, EnvironmentID: environmentID,
	})
}

// LockRevisionForZoneShare share-locks one zone's latest revision.
func (p *ProviderTx) LockRevisionForZoneShare(ctx context.Context, provider, zoneID string) (uint64, bool, error) {
	revision, err := p.owner.q.LockProviderRevisionForZoneShare(ctx, query.LockProviderRevisionForZoneShareParams{Provider: provider, ZoneID: zoneID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, err := providerRevisionValue(revision)
	return value, err == nil, err
}
