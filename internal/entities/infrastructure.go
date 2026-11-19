package entities

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	User, Database, Schema             string
	ServerVersion                      int
	TLS                                bool
	CanCreateDatabase, CanCreateSchema bool
}

// SameDatabase tests whether two endpoints share a database's advisory lock
// namespace, without relying on matching hostnames or privileged catalog reads.
func SameDatabase(ctx context.Context, first, second *pgxpool.Pool) (bool, error) {
	a, err := first.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollbackDatabaseIdentityProbe(ctx, a)
	b, err := second.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollbackDatabaseIdentityProbe(ctx, b)
	var raw [8]byte
	rand.Read(raw[:])
	lockKey := int64(binary.LittleEndian.Uint64(raw[:])) //nolint:gosec // All signed lock keys are valid.
	locked, err := query.New(a).TryDatabaseIdentityLock(ctx, lockKey)
	if err != nil || !locked {
		return false, err
	}
	other, err := query.New(b).TryDatabaseIdentityLock(ctx, lockKey)
	return !other, err
}

func rollbackDatabaseIdentityProbe(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (s *Store) DatabaseVersion(ctx context.Context) (int, error) {
	version, err := query.New(s.pool).DatabaseVersion(ctx)
	return int(version), err
}

func (s *Store) DatabaseFacts(ctx context.Context) (DatabaseFacts, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return DatabaseFacts{}, err
	}
	defer conn.Release()
	row, err := query.New(conn).DatabaseFacts(ctx)
	// pg_stat_ssl describes the backend hop, not our transport when a
	// pooler terminates TLS.
	wire, _ := conn.Conn().PgConn().Conn().(*tls.Conn)
	return DatabaseFacts{
		User: row.RoleName, Database: row.DatabaseName, Schema: row.SchemaName, ServerVersion: int(row.ServerVersion),
		TLS:               wire != nil && wire.ConnectionState().HandshakeComplete,
		CanCreateDatabase: row.CanCreateDatabase, CanCreateSchema: row.CanCreateSchema,
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
