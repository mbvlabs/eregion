package integrations

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// PostgresGrantStore shares access state between all API and worker processes.
// Migration 005 owns its schema; construction does not run DDL.
type PostgresGrantStore struct{ pool *pgxpool.Pool }

func NewPostgresGrantStore(pool *pgxpool.Pool) (*PostgresGrantStore, error) {
	if pool == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "access storage requires PostgreSQL", "Open the control-plane database first.")
	}
	return &PostgresGrantStore{pool: pool}, nil
}

func (s *PostgresGrantStore) Get(ctx context.Context, id string) (DeviceGrant, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT record FROM eregion_access_grants WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceGrant{}, false, nil
	}
	if err != nil {
		return DeviceGrant{}, false, storeUnavailable(err)
	}
	var grant DeviceGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return grant, false, storeUnavailable(err)
	}
	return grant, true, nil
}

func (s *PostgresGrantStore) List(ctx context.Context) ([]DeviceGrant, error) {
	rows, err := s.pool.Query(ctx, "SELECT record FROM eregion_access_grants ORDER BY id")
	if err != nil {
		return nil, storeUnavailable(err)
	}
	defer rows.Close()
	result := []DeviceGrant{}
	for rows.Next() {
		var raw []byte
		var grant DeviceGrant
		if err := rows.Scan(&raw); err != nil {
			return nil, storeUnavailable(err)
		}
		if err := json.Unmarshal(raw, &grant); err != nil {
			return nil, storeUnavailable(err)
		}
		result = append(result, grant)
	}
	return result, rows.Err()
}

// Put never replaces an existing grant on a creation retry and never revives a
// revoked grant. Revocation updates only revocation fields under a row lock.
func (s *PostgresGrantStore) Put(ctx context.Context, grant DeviceGrant) error {
	if !grantIDPattern.MatchString(grant.ID) {
		return contract.NewFault(contract.ValidationFailed, "invalid access grant ID", "Use the grant identity derived from the operation.")
	}
	raw, err := json.Marshal(grant)
	if err != nil {
		return storeUnavailable(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storeUnavailable(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "INSERT INTO eregion_access_grants (id,record) VALUES ($1,$2) ON CONFLICT DO NOTHING", grant.ID, raw); err != nil {
		return storeUnavailable(err)
	}
	var existingRaw []byte
	if err := tx.QueryRow(ctx, "SELECT record FROM eregion_access_grants WHERE id=$1 FOR UPDATE", grant.ID).Scan(&existingRaw); err != nil {
		return storeUnavailable(err)
	}
	var existing DeviceGrant
	if err := json.Unmarshal(existingRaw, &existing); err != nil {
		return storeUnavailable(err)
	}
	if !sameGrantRequest(existing, grant, true) {
		return contract.NewFault(contract.Conflict, "grant identity already binds different input", "Retry the original operation unchanged.")
	}
	if existing.RevokedAt == nil && grant.RevokedAt != nil {
		existing.RevokedAt, existing.RevokedBy, existing.RevokedByOperation = grant.RevokedAt, grant.RevokedBy, grant.RevokedByOperation
		raw, _ = json.Marshal(existing)
		if _, err := tx.Exec(ctx, "UPDATE eregion_access_grants SET record=$2 WHERE id=$1", grant.ID, raw); err != nil {
			return storeUnavailable(err)
		}
	}
	return tx.Commit(ctx)
}

// PostgresAudit is a synchronous durable audit sink, not an execution queue.
type PostgresAudit struct{ pool *pgxpool.Pool }

func NewPostgresAudit(pool *pgxpool.Pool) (*PostgresAudit, error) {
	if pool == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "audit storage requires PostgreSQL", "Open the control-plane database first.")
	}
	return &PostgresAudit{pool: pool}, nil
}

func (s *PostgresAudit) Record(ctx context.Context, entry AuditEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return storeUnavailable(err)
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO eregion_integration_audit (entry) VALUES ($1)", raw); err != nil {
		return contract.NewFault(contract.Unavailable, "durable integration audit is unavailable", "Repair database connectivity before performing the action.")
	}
	return nil
}
