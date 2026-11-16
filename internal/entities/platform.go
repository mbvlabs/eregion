package entities

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// PlatformStore is the typed platform persistence capability over the Store's
// borrowed pool. It exposes no pool, transaction, query or driver handle.
type PlatformStore struct {
	store *Store
	q     *query.Queries
}

// PlatformTx is the same capability bound to one Store transaction.
type PlatformTx struct {
	owner *StoreTx
	q     *query.Queries
}

// Platform returns the platform state view of this Store.
func (s *Store) Platform() *PlatformStore { return &PlatformStore{store: s, q: query.New(s.pool)} }

// Platform returns the platform state view of this transaction.
func (tx *StoreTx) Platform() *PlatformTx { return &PlatformTx{owner: tx, q: tx.q} }

// Resources returns the managed-resource view of this Store.
func (s *Store) Resources() *ResourceStore { return &ResourceStore{q: query.New(s.pool)} }

// Resources returns the managed-resource view of this transaction.
func (tx *StoreTx) Resources() *ResourceStore { return &ResourceStore{q: tx.q} }

// Nodes returns the node view of this Store.
func (s *Store) Nodes() *NodeStore { return &NodeStore{store: s, q: query.New(s.pool)} }

// Nodes returns the node view of this transaction.
func (tx *StoreTx) Nodes() *NodeStore { return &NodeStore{q: tx.q} }

func (p *PlatformStore) Apps() *AppStore                       { return &AppStore{q: p.q} }
func (p *PlatformStore) Environments() *EnvironmentStore       { return &EnvironmentStore{q: p.q} }
func (p *PlatformStore) Revisions() *RevisionStore             { return &RevisionStore{q: p.q} }
func (p *PlatformStore) Releases() *ReleaseStore               { return &ReleaseStore{q: p.q} }
func (p *PlatformStore) Builds() *BuildStore                   { return &BuildStore{q: p.q} }
func (p *PlatformStore) Deployments() *DeploymentStore         { return &DeploymentStore{q: p.q} }
func (p *PlatformStore) Domains() *DomainStore                 { return &DomainStore{q: p.q} }
func (p *PlatformStore) DomainDNS() *DomainDNSStore            { return &DomainDNSStore{q: p.q} }
func (p *PlatformStore) Changes() *ChangeStore                 { return &ChangeStore{q: p.q} }
func (p *PlatformStore) Owners() *DeploymentOwnerStore         { return &DeploymentOwnerStore{q: p.q} }
func (p *PlatformStore) Cleanup() *ContainerCleanupStore       { return &ContainerCleanupStore{q: p.q} }
func (p *PlatformStore) ReleaseCommands() *ReleaseCommandStore { return &ReleaseCommandStore{q: p.q} }
func (p *PlatformStore) Pointers() *PointerStore               { return &PointerStore{q: p.q} }
func (p *PlatformStore) Locks() *LockStore                     { return &LockStore{store: p} }
func (p *PlatformStore) Inventory() *InventoryStore            { return &InventoryStore{q: p.q} }
func (p *PlatformStore) Outcomes() *OutcomeStore               { return &OutcomeStore{q: p.q} }

func (p *PlatformTx) Apps() *AppStore                       { return &AppStore{q: p.q} }
func (p *PlatformTx) Environments() *EnvironmentStore       { return &EnvironmentStore{q: p.q} }
func (p *PlatformTx) Revisions() *RevisionStore             { return &RevisionStore{q: p.q} }
func (p *PlatformTx) Releases() *ReleaseStore               { return &ReleaseStore{q: p.q} }
func (p *PlatformTx) Builds() *BuildStore                   { return &BuildStore{q: p.q} }
func (p *PlatformTx) Deployments() *DeploymentStore         { return &DeploymentStore{q: p.q} }
func (p *PlatformTx) Domains() *DomainStore                 { return &DomainStore{q: p.q} }
func (p *PlatformTx) DomainDNS() *DomainDNSStore            { return &DomainDNSStore{q: p.q} }
func (p *PlatformTx) Changes() *ChangeStore                 { return &ChangeStore{q: p.q} }
func (p *PlatformTx) Owners() *DeploymentOwnerStore         { return &DeploymentOwnerStore{q: p.q} }
func (p *PlatformTx) Cleanup() *ContainerCleanupStore       { return &ContainerCleanupStore{q: p.q} }
func (p *PlatformTx) ReleaseCommands() *ReleaseCommandStore { return &ReleaseCommandStore{q: p.q} }
func (p *PlatformTx) Pointers() *PointerStore               { return &PointerStore{q: p.q} }
func (p *PlatformTx) Locks() *LockStore                     { return &LockStore{tx: p} }
func (p *PlatformTx) Inventory() *InventoryStore            { return &InventoryStore{q: p.q} }
func (p *PlatformTx) Outcomes() *OutcomeStore               { return &OutcomeStore{q: p.q} }

// timestampAny accepts the two timestamp shapes pgx uses for an untyped
// clock_timestamp() column.
func timestampAny(value any) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), nil
	case nil:
		return time.Time{}, nil
	default:
		return time.Time{}, ErrPlatformCorrupt
	}
}

func platformTime(value pgtype.Timestamptz) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, ErrPlatformCorrupt
	}
	return value.Time.UTC(), nil
}

func platformTimePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time.UTC()
	return &at
}

func platformNullableTime(value pgtype.Timestamptz) any {
	if !value.Valid {
		return nil
	}
	return value.Time.UTC()
}

func platformInt(value int64) (int, error) {
	converted := int(value)
	if int64(converted) != value {
		return 0, ErrPlatformCorrupt
	}
	return converted, nil
}

func platformUint32(value int64) (uint32, error) {
	if value < 0 || uint64(value) > uint64(^uint32(0)) {
		return 0, ErrPlatformCorrupt
	}
	return uint32(value), nil
}

func platformUint64(value int64) (uint64, error) {
	if value < 0 {
		return 0, ErrPlatformCorrupt
	}
	return uint64(value), nil
}

func platformUint64FromInt32(value int32) (uint64, error) {
	if value < 0 {
		return 0, ErrPlatformCorrupt
	}
	return uint64(value), nil
}

func platformInt64(value uint64) (int64, error) {
	converted := int64(value)
	if converted < 0 || uint64(converted) != value {
		return 0, ErrPlatformCorrupt
	}
	return converted, nil
}

func platformJSONMap(raw json.RawMessage) (map[string]string, error) {
	result := map[string]string{}
	if len(raw) == 0 || string(raw) == "null" {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, ErrPlatformCorrupt
	}
	return result, nil
}

func platformJSON[T any](raw json.RawMessage) (T, error) {
	var result T
	if len(raw) == 0 || string(raw) == "null" {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, ErrPlatformCorrupt
	}
	return result, nil
}

func platformOptionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func platformText(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func platformOptionalInt64(value int) (int64, error) {
	return int64(value), nil
}

var ErrPlatformLeaseUncertain = errors.New("entities: platform session lease unlock is uncertain")
