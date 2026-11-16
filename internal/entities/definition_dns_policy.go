package entities

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// DefinitionPolicyLockKey serializes definition DNS policy configuration with
// definition admission. Both paths must acquire it before any application or
// policy row lock.
const DefinitionPolicyLockKey int64 = 0x657265646e73706c

// DefinitionDNSPolicyRecord is one non-secret policy row. Set flags preserve
// the difference between a NULL provider binding and an empty string so
// corrupted policies fail exactly as before.
type DefinitionDNSPolicyRecord struct {
	Revision    uint64
	Enabled     bool
	Provider    string
	ProviderSet bool
	ZoneID      string
	ZoneIDSet   bool
	ZoneName    string
	ZoneNameSet bool
	Origins     []string
	ProxyMode   string
}

// DefinitionDNSPolicyStore is the read view of the singleton definition DNS
// policy. It borrows the Store's pool and exposes no raw query handle.
type DefinitionDNSPolicyStore struct{ q *query.Queries }

// DefinitionDNSPolicy returns the definition DNS policy read view of this Store.
func (s *Store) DefinitionDNSPolicy() *DefinitionDNSPolicyStore {
	return &DefinitionDNSPolicyStore{q: query.New(s.pool)}
}

// Get reads the singleton policy. A missing row is (zero, false, nil); the
// caller owns the typed revision-zero disabled default.
func (p *DefinitionDNSPolicyStore) Get(ctx context.Context) (DefinitionDNSPolicyRecord, bool, error) {
	row, err := p.q.GetDefinitionDNSPolicy(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSPolicyRecord{}, false, nil
	}
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	record, err := definitionPolicyRecord(row.Revision, row.Enabled, row.Provider, row.ZoneID, row.ZoneName, row.Origins, row.ProxyMode)
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	return record, true, nil
}

// DefinitionDNSPolicyTx is the policy view of one StoreTx. Methods use the
// caller's transaction and never start another one.
type DefinitionDNSPolicyTx struct{ owner *StoreTx }

// DefinitionDNSPolicy returns the policy view of this transaction.
func (tx *StoreTx) DefinitionDNSPolicy() *DefinitionDNSPolicyTx {
	return &DefinitionDNSPolicyTx{owner: tx}
}

// LockWriters acquires the advisory transaction lock shared with definition
// admission. It remains held until commit or rollback.
func (p *DefinitionDNSPolicyTx) LockWriters(ctx context.Context) error {
	return p.owner.q.LockDefinitionPolicyWriters(ctx, DefinitionPolicyLockKey)
}

// ActiveDefinitionRun reports whether any definition run is unfinished.
func (p *DefinitionDNSPolicyTx) ActiveDefinitionRun(ctx context.Context) (bool, error) {
	return p.owner.q.ActiveDefinitionRunExists(ctx)
}

// EnsureSingleton inserts the disabled revision-zero row when it is absent. The
// caller's advisory lock makes insertion and revision comparison one serialized
// operation.
func (p *DefinitionDNSPolicyTx) EnsureSingleton(ctx context.Context, origins []string, proxyMode string, updatedAt time.Time) error {
	return p.owner.q.EnsureDefinitionDNSPolicySingleton(ctx, query.EnsureDefinitionDNSPolicySingletonParams{
		Origins: origins, ProxyMode: proxyMode,
		UpdatedAt: pgtype.Timestamptz{Time: updatedAt, Valid: true},
	})
}

// Lock locks and reads the singleton policy for the revision CAS.
func (p *DefinitionDNSPolicyTx) Lock(ctx context.Context) (DefinitionDNSPolicyRecord, bool, error) {
	row, err := p.owner.q.LockDefinitionDNSPolicy(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSPolicyRecord{}, false, nil
	}
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	record, err := definitionPolicyRecord(row.Revision, row.Enabled, row.Provider, row.ZoneID, row.ZoneName, row.Origins, row.ProxyMode)
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	return record, true, nil
}

// LockProviderZones takes a share lock on each exact provider credential that
// matches the zone ID or zone name reference. The caller requires exactly one.
func (p *DefinitionDNSPolicyTx) LockProviderZones(ctx context.Context, zoneRef string) ([]string, error) {
	return p.owner.q.LockDefinitionPolicyProviderZones(ctx, zoneRef)
}

// Enable writes an explicit enabled policy with the provider binding supplied
// by the caller's locked lookup.
func (p *DefinitionDNSPolicyTx) Enable(ctx context.Context, zoneID string, origins []string, proxyMode string) error {
	return p.owner.q.EnableDefinitionDNSPolicy(ctx, query.EnableDefinitionDNSPolicyParams{
		ZoneID: zoneID, Origins: origins, ProxyMode: proxyMode,
	})
}

// Disable increments the revision while retaining prior settings.
func (p *DefinitionDNSPolicyTx) Disable(ctx context.Context) error {
	return p.owner.q.DisableDefinitionDNSPolicy(ctx)
}

// Policy reads the committed-in-transaction policy for the response. It does
// not lock; the caller already holds the policy row lock.
func (p *DefinitionDNSPolicyTx) Policy(ctx context.Context) (DefinitionDNSPolicyRecord, bool, error) {
	row, err := p.owner.q.GetDefinitionDNSPolicy(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSPolicyRecord{}, false, nil
	}
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	record, err := definitionPolicyRecord(row.Revision, row.Enabled, row.Provider, row.ZoneID, row.ZoneName, row.Origins, row.ProxyMode)
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	return record, true, nil
}

func definitionPolicyRecord(revision int64, enabled bool, provider, zoneID, zoneName pgtype.Text, origins []string, proxyMode string) (DefinitionDNSPolicyRecord, error) {
	if revision < 0 {
		return DefinitionDNSPolicyRecord{}, contract.NewFault(contract.Unavailable, "definition DNS policy revision is outside the supported range", "Repair the stored policy before using definition DNS.")
	}
	return DefinitionDNSPolicyRecord{
		Revision: uint64(revision), Enabled: enabled,
		Provider: definitionPolicyText(provider), ProviderSet: provider.Valid,
		ZoneID: definitionPolicyText(zoneID), ZoneIDSet: zoneID.Valid,
		ZoneName: definitionPolicyText(zoneName), ZoneNameSet: zoneName.Valid,
		Origins: append([]string{}, origins...), ProxyMode: proxyMode,
	}, nil
}

func definitionPolicyText(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

// LockShared share-locks the policy row, matching definition admission and
// planning reads. Configuration writes keep the stronger FOR UPDATE lock.
func (p *DefinitionDNSPolicyTx) LockShared(ctx context.Context) (DefinitionDNSPolicyRecord, bool, error) {
	row, err := p.owner.q.LockDefinitionDNSPolicyShared(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionDNSPolicyRecord{}, false, nil
	}
	if err != nil {
		return DefinitionDNSPolicyRecord{}, false, err
	}
	record, err := definitionPolicyRecord(row.Revision, row.Enabled, row.Provider, row.ZoneID, row.ZoneName, row.Origins, row.ProxyMode)
	return record, err == nil, err
}
