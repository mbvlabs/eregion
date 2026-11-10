package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
)

// Workspace identity is installation metadata, not a tenant boundary. The
// database owns the one configured identity; node configuration and operator
// access files may mirror it but never decide it. An empty migrated table is
// explicitly unconfigured until initialization assigns its identity.
var (
	workspaceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	workspaceIDPattern   = regexp.MustCompile(`^ws_[0-9a-f]{24}$`)
	// ErrWorkspaceSchemaMissing signals that the workspace migration has not run.
	ErrWorkspaceSchemaMissing = errors.New("workspace schema is missing")
)

// workspaceAssignmentLock serialises the singleton insert and replay without
// holding a table lock. It is intentionally unrelated to migrationLock.
const workspaceAssignmentLock int64 = 0x65726567776f726b // "eregwork"

// WorkspaceIdentity is the portable, non-secret installation identity. The
// zero value is the explicit unconfigured state; it is never treated as a
// guess at a workspace name or ID.
type WorkspaceIdentity struct {
	Configured bool   `json:"configured"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ValidateWorkspaceName checks the human-readable slug before any database
// transaction or local state write.
func ValidateWorkspaceName(name string) error {
	if !workspaceNamePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid workspace name "+name,
			"Use 1–64 lowercase letters, digits, or hyphens, starting and ending with a letter or digit (for example thelab).")
	}
	return nil
}

// ValidateWorkspaceID checks the generated portable ID in bundles and access records.
func ValidateWorkspaceID(id string) error {
	if !workspaceIDPattern.MatchString(id) {
		return contract.NewFault(contract.ValidationFailed, "invalid workspace ID "+id,
			"Use the workspace ID from a current protected export; do not edit it by hand.")
	}
	return nil
}

// NewWorkspaceID generates the immutable portable ID at first assignment.
func NewWorkspaceID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", contract.NewFault(contract.Internal, "could not generate a workspace ID", "Retry the apply on the node.")
	}
	return "ws_" + hex.EncodeToString(raw[:]), nil
}

type workspaceQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func workspaceTableExists(ctx context.Context, pool workspaceQuerier) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('eregion_workspace') IS NOT NULL").Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// ReadWorkspace reads the authoritative identity without inserting a row.
// A missing table is distinct from a migrated but empty table.
func ReadWorkspace(ctx context.Context, pool *pgxpool.Pool) (WorkspaceIdentity, error) {
	return readWorkspaceSchema(ctx, pool)
}

func readWorkspaceSchema(ctx context.Context, pool workspaceQuerier) (WorkspaceIdentity, error) {
	exists, err := workspaceTableExists(ctx, pool)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if !exists {
		return WorkspaceIdentity{}, ErrWorkspaceSchemaMissing
	}
	return readWorkspace(ctx, pool)
}

func readWorkspace(ctx context.Context, q workspaceQuerier) (WorkspaceIdentity, error) {
	var identity WorkspaceIdentity
	err := q.QueryRow(ctx, `SELECT workspace_id, name FROM eregion_workspace WHERE singleton`).
		Scan(&identity.ID, &identity.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceIdentity{}, nil
	}
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if err := ValidateWorkspaceID(identity.ID); err != nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the control database has an invalid workspace ID", "Inspect the workspace row; never rewrite applied migrations.")
	}
	if err := ValidateWorkspaceName(identity.Name); err != nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the control database has an invalid workspace name", "Inspect the workspace row; never rewrite applied migrations.")
	}
	identity.Configured = true
	return identity, nil
}

// AssignWorkspace implements the one-time compare-and-set assignment:
//
//   - an unconfigured installation gets a newly generated immutable ID;
//   - replaying the same name returns the same identity;
//   - a different name is refused and never overwrites the existing identity.
//
// The advisory transaction lock serialises assignment. INSERT ... ON CONFLICT
// is the compare-and-set that also handles a writer that did not take the lock.
func AssignWorkspace(ctx context.Context, pool *pgxpool.Pool, name string) (WorkspaceIdentity, error) {
	if err := ValidateWorkspaceName(name); err != nil {
		return WorkspaceIdentity{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", workspaceAssignmentLock); err != nil {
		return WorkspaceIdentity{}, err
	}
	id, err := NewWorkspaceID()
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO eregion_workspace (singleton, workspace_id, name) VALUES (true, $1, $2)
         ON CONFLICT (singleton) DO NOTHING`, id, name); err != nil {
		return WorkspaceIdentity{}, err
	}
	identity, err := readWorkspace(ctx, tx)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if !identity.Configured {
		return WorkspaceIdentity{}, contract.NewFault(contract.Internal,
			"the workspace identity was not recorded", "Inspect the control database and retry the apply.")
	}
	if identity.Name != name {
		return WorkspaceIdentity{}, workspaceNameConflict(identity.Name, name)
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceIdentity{}, err
	}
	return identity, nil
}

func workspaceNameConflict(configured, requested string) error {
	fault := contract.NewFault(contract.Conflict,
		"the workspace identity is already configured as "+configured,
		"The workspace name and ID are immutable. Use the existing name "+configured+", or restore the intended database before assigning another workspace.")
	fault.Details = map[string]any{
		"configured_workspace": configured,
		"requested_workspace":  requested,
	}
	return fault
}
