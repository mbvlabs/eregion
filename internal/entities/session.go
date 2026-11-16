package entities

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// SessionStore binds the typed views to one dedicated connection whose
// session-level advisory locks must outlive any transaction. It exposes no
// query text, row scanner or transaction handle.
type SessionStore struct {
	conn *pgx.Conn
	q    *query.Queries
}

// NewSessionStore wraps a caller-owned connection. The caller still owns the
// lifetime of the connection and must close it.
func NewSessionStore(conn *pgx.Conn) *SessionStore {
	if conn == nil {
		return nil
	}
	return &SessionStore{conn: conn, q: query.New(conn)}
}

// Workspace returns the typed singleton workspace view on this session.
func (s *SessionStore) Workspace() *WorkspaceStore { return &WorkspaceStore{q: s.q} }

// Apps returns the typed application view on this session.
func (s *SessionStore) Apps() *AppStore { return &AppStore{q: s.q} }

// TrySessionLock takes one session advisory lock without waiting. The lock is
// released by UnlockSession or when the connection closes.
func (s *SessionStore) TrySessionLock(ctx context.Context, key int64) (bool, error) {
	return s.q.TryPlatformSessionLock(ctx, key)
}

// UnlockSession releases one session advisory lock.
func (s *SessionStore) UnlockSession(ctx context.Context, key int64) (bool, error) {
	return s.q.ReleasePlatformSessionLock(ctx, key)
}

// Close releases the underlying connection and every session lock it holds.
func (s *SessionStore) Close(ctx context.Context) error { return s.conn.Close(ctx) }
