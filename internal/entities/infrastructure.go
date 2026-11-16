package entities

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/mbvlabs/eregion/internal/entities/internal/infrastructurequery"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// DatabaseIdentity describes the session search path and role, without
// exposing credentials or a driver handle.
type DatabaseIdentity struct {
	SchemaCount int
	Schema      string
	Role        string
}

type DatabaseFacts struct {
	User, Database                     string
	ServerVersion                      int
	TLS                                bool
	CanCreateDatabase, CanCreateSchema bool
}

func (s *Store) DatabaseVersion(ctx context.Context) (int, error) {
	version, err := query.New(s.pool).DatabaseVersion(ctx)
	return int(version), err
}

func (s *Store) DatabaseFacts(ctx context.Context) (DatabaseFacts, error) {
	row, err := query.New(s.pool).DatabaseFacts(ctx)
	return DatabaseFacts{
		User: row.RoleName, Database: row.DatabaseName, ServerVersion: int(row.ServerVersion),
		TLS: row.Tls, CanCreateDatabase: row.CanCreateDatabase, CanCreateSchema: row.CanCreateSchema,
	}, err
}

func (s *SessionStore) DatabaseIdentity(ctx context.Context) (DatabaseIdentity, error) {
	row, err := s.q.DatabaseIdentity(ctx)
	return DatabaseIdentity{SchemaCount: int(row.SchemaCount), Schema: row.SchemaName, Role: row.RoleName}, err
}

// MigrationSnapshot binds application catalog reads to Goose's existing
// repeatable-read database/sql transaction. It does not begin a transaction,
// own a connection, or offer an alternate persistence path for domain data.
type MigrationSnapshot struct {
	q *infrastructurequery.Queries
}

func NewMigrationSnapshot(tx *sql.Tx) *MigrationSnapshot {
	return &MigrationSnapshot{q: infrastructurequery.New(tx)}
}

type MigrationCatalog struct {
	Identity                   DatabaseIdentity
	LegacyLedger, LedgerExists bool
}

func (s *MigrationSnapshot) Catalog(ctx context.Context) (MigrationCatalog, error) {
	row, err := s.q.ApplicationMigrationCatalog(ctx)
	return MigrationCatalog{
		Identity:     DatabaseIdentity{SchemaCount: int(row.SchemaCount), Schema: row.SchemaName, Role: row.RoleName},
		LegacyLedger: row.LegacyLedger, LedgerExists: row.LedgerExists,
	}, err
}

func (s *MigrationSnapshot) UnexpectedObjects(ctx context.Context, tables, indexes []string, applicationPending bool) (bool, error) {
	// JSON transports the exact identifier sets through database/sql without
	// introducing a second PostgreSQL driver just to encode text arrays.
	if tables == nil {
		tables = []string{}
	}
	if indexes == nil {
		indexes = []string{}
	}
	tableJSON, err := json.Marshal(tables)
	if err != nil {
		return false, err
	}
	indexJSON, err := json.Marshal(indexes)
	if err != nil {
		return false, err
	}
	return s.q.UnexpectedMigrationObjects(ctx, infrastructurequery.UnexpectedMigrationObjectsParams{
		ApplicationPending: applicationPending, Tables: string(tableJSON), Indexes: string(indexJSON),
	})
}
