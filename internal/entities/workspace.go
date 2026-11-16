package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// WorkspaceRecord is the persisted workspace identity. Registry-host fields are
// nullable as a group in the database; the caller decides whether a partial
// binding is acceptable. No schema-existence or assignment policy lives here.
type WorkspaceRecord struct {
	ID                         string
	Name                       string
	RegistryHostNode           *string
	RegistryHostKeyType        *string
	RegistryHostKeyFingerprint *string
	RegistryHostStateDir       *string
}

// WorkspaceInsert is one immutable identity assignment attempt. An empty
// RegistryHost option inserts SQL NULLs.
type WorkspaceInsert struct {
	ID                         string
	Name                       string
	RegistryHostNode           string
	RegistryHostKeyType        string
	RegistryHostKeyFingerprint string
	RegistryHostStateDir       string
}

// WorkspaceRegistryHost returns true when every registry-host field is present.
func (w WorkspaceRecord) WorkspaceRegistryHost() bool {
	return w.RegistryHostNode != nil && w.RegistryHostKeyType != nil &&
		w.RegistryHostKeyFingerprint != nil && w.RegistryHostStateDir != nil
}

// WorkspaceRegistryHostPartial reports a partially populated binding. The
// caller treats it as corrupt; this value is a neutral fact, not a decision.
func (w WorkspaceRecord) WorkspaceRegistryHostPartial() bool {
	present := 0
	for _, value := range []*string{w.RegistryHostNode, w.RegistryHostKeyType, w.RegistryHostKeyFingerprint, w.RegistryHostStateDir} {
		if value != nil {
			present++
		}
	}
	return present != 0 && present != 4
}

// WorkspaceStore is the read view of the workspace identity.
type WorkspaceStore struct{ q *query.Queries }

// Workspace returns the workspace identity read view of this Store.
func (s *Store) Workspace() *WorkspaceStore { return &WorkspaceStore{q: query.New(s.pool)} }

// WorkspaceTx is the workspace view of one StoreTx. Reads and the one-time
// insert use the caller's transaction.
type WorkspaceTx struct{ q *query.Queries }

// Workspace returns the workspace view of this transaction.
func (tx *StoreTx) Workspace() *WorkspaceTx { return &WorkspaceTx{q: tx.q} }

// SchemaExists reports whether the workspace identity table exists. It is the
// typed catalog observation that keeps the schema check free of raw SQL.
func (s *WorkspaceStore) SchemaExists(ctx context.Context) (bool, error) {
	return s.q.WorkspaceSchemaExists(ctx)
}

// SchemaExists reads the same catalog fact inside the caller's transaction.
func (tx *WorkspaceTx) SchemaExists(ctx context.Context) (bool, error) {
	return tx.q.WorkspaceSchemaExists(ctx)
}

// LockAssignment takes the transaction-scoped advisory lock that serializes
// first assignment and replay. Policy remains with the caller.
func (tx *WorkspaceTx) LockAssignment(ctx context.Context, key int64) error {
	return tx.q.LockWorkspaceAssignment(ctx, key)
}

// Get reads the singleton identity. A migrated but empty table is (zero,false).
func (s *WorkspaceStore) Get(ctx context.Context) (WorkspaceRecord, bool, error) {
	return workspaceGet(ctx, s.q)
}

// Get reads the singleton identity inside the caller's transaction.
func (tx *WorkspaceTx) Get(ctx context.Context) (WorkspaceRecord, bool, error) {
	return workspaceGet(ctx, tx.q)
}

func workspaceGet(ctx context.Context, q *query.Queries) (WorkspaceRecord, bool, error) {
	row, err := q.GetWorkspaceIdentity(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceRecord{}, false, nil
	}
	if err != nil {
		return WorkspaceRecord{}, false, err
	}
	return WorkspaceRecord{
		ID: row.WorkspaceID, Name: row.Name,
		RegistryHostNode:           workspaceOptional(row.RegistryHostNode),
		RegistryHostKeyType:        workspaceOptional(row.RegistryHostKeyType),
		RegistryHostKeyFingerprint: workspaceOptional(row.RegistryHostKeyFingerprint),
		RegistryHostStateDir:       workspaceOptional(row.RegistryHostStateDir),
	}, true, nil
}

// Insert records the first identity. ON CONFLICT DO NOTHING is the database
// compare-and-set; false means another row already exists and must be re-read.
func (tx *WorkspaceTx) Insert(ctx context.Context, insert WorkspaceInsert) (bool, error) {
	if insert.ID == "" || insert.Name == "" {
		return false, errors.New("entities: workspace identity requires id and name")
	}
	rows, err := tx.q.InsertWorkspaceIdentity(ctx, query.InsertWorkspaceIdentityParams{
		WorkspaceID: insert.ID, Name: insert.Name,
		RegistryHostNode:           workspaceText(insert.RegistryHostNode),
		RegistryHostKeyType:        workspaceText(insert.RegistryHostKeyType),
		RegistryHostKeyFingerprint: workspaceText(insert.RegistryHostKeyFingerprint),
		RegistryHostStateDir:       workspaceText(insert.RegistryHostStateDir),
	})
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func workspaceOptional(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	v := value.String
	return &v
}

func workspaceText(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}
